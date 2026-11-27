package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func (n *Node) restoreFromStore(ctx context.Context) error {
	if n.marketLS == nil {
		return nil
	}
	isLive := n.net == "main" || n.net == "testnet"
	var cursor int64
	for {
		events, err := n.store.EventsSince(cursor, nil, 1000)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Seq > cursor {
				cursor = ev.Seq
			}
			if len(ev.BEEF) == 0 {
				continue
			}
			var tx *transaction.Transaction
			if isLive {
				_, parsed, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
				if pErr != nil || parsed == nil || int(ev.Vout) >= len(parsed.Outputs) {
					continue
				}
				if n.tracker != nil {
					if verdict, _ := verify.Check(ctx, parsed, n.tracker); verdict != verify.Passed {
						continue
					}
				}
				tx = parsed
			} else {
				parsed, err := transaction.NewTransactionFromBEEF(ev.BEEF)
				if err != nil || parsed == nil || int(ev.Vout) >= len(parsed.Outputs) {
					continue
				}
				tx = parsed
			}
			out := tx.Outputs[ev.Vout]
			op := transaction.Outpoint{Txid: *tx.TxID(), Index: ev.Vout}
			_ = n.marketLS.OutputAdmittedByTopic(ctx, &module.OutputAdmittedByTopic{
				Mode:          module.AdmitLockingScript,
				Outpoint:      op,
				Topic:         ev.Topic,
				Satoshis:      out.Satoshis,
				LockingScript: out.LockingScript,
				AtomicBEEF:    ev.BEEF,
			})
			n.marketLS.SetSeq(op.String(), ev.Seq)
		}
		if len(events) < 1000 {
			break
		}
	}
	return nil
}

// HandleReorg evicts persisted Hub events anchored at block_height >= forkHeight and notifies
// every mounted LookupService via OutputEvicted so orphaned block state is rolled back cleanly.
func (n *Node) HandleReorg(ctx context.Context, forkHeight uint32) error {
	if forkHeight == 0 {
		return nil
	}
	evicted, err := n.store.EvictEventsFromHeight(forkHeight)
	if err != nil {
		return err
	}
	for _, ev := range evicted {
		h, hErr := chainhash.NewHashFromHex(ev.TxID)
		if hErr != nil || h == nil {
			continue
		}
		op := transaction.Outpoint{Txid: *h, Index: ev.Vout}
		for _, ls := range n.lookups {
			_ = ls.OutputEvicted(ctx, &op)
		}
	}
	return nil
}

func extractMeta(p bstack.MarketPayload) (pub string, domains []string, exp int64) {
	switch p.Kind {
	case bstack.KindWant:
		var sw bstack.SignedWant
		if json.Unmarshal(p.Body, &sw) == nil {
			return sw.Want.Pub, sw.Want.Domains, sw.Want.Exp
		}
	case bstack.KindOffer:
		var so bstack.SignedOffer
		if json.Unmarshal(p.Body, &so) == nil {
			return so.Offer.Pub, so.Offer.Domains, so.Offer.Exp
		}
	case bstack.KindAnnounce:
		var sa bstack.SignedAnnounce
		if json.Unmarshal(p.Body, &sa) == nil {
			return sa.Announce.Pub, append([]string{bstack.DirDomain}, sa.Announce.Domains...), sa.Announce.Exp
		}
	}
	return "", nil, 0
}

// onMarketAdmit is called by ls_nektar on admission; live fanout is triggered by Submit after durable seq assignment.
func (n *Node) onMarketAdmit(_ bstack.MarketEvent) {}

// classifySubmitPayload validates a POST /submit BEEF and market payload BEFORE charging any fee
// (Lightweb validate-before-charge rule: malformed payloads return 400 with zero sats taken).
// Returns kind=="" for valid non-market Lightweb module submissions (bfinger, bbox, bstore, etc.).
func (n *Node) classifySubmitPayload(rawBEEF []byte, topics []string) (bstack.ArtifactKind, error) {
	if len(rawBEEF) == 0 {
		return "", errors.New("hub: empty BEEF body")
	}
	if len(rawBEEF) > 4<<20 {
		return "", errors.New("hub: BEEF exceeds 4 MiB limit")
	}
	isLive := n.net == "main" || n.net == "testnet"
	allMarketTopics := len(topics) > 0
	for _, t := range topics {
		if !strings.HasPrefix(t, "tm_nektar_") {
			allMarketTopics = false
			break
		}
	}
	var tx *transaction.Transaction
	_, parsedTx, _, err := guard.ParseBEEF(rawBEEF, guard.DefaultBound)
	if err == nil && parsedTx != nil {
		tx = parsedTx
	} else if !isLive && allMarketTopics && err != nil && strings.Contains(err.Error(), "no inputs") && len(rawBEEF) <= 64<<10 {
		t0, tErr := transaction.NewTransactionFromBEEF(rawBEEF)
		if tErr != nil || t0 == nil || len(t0.Inputs) != 0 || len(t0.Outputs) == 0 || len(t0.Outputs) > 16 {
			return "", fmt.Errorf("hub: parse carrier BEEF: %w", err)
		}
		tx = t0
	} else {
		if err == nil {
			err = errors.New("empty BEEF transaction")
		}
		return "", fmt.Errorf("hub: parse BEEF: %w", err)
	}
	if !allMarketTopics {
		// Check if any requested topic is a market topic.
		hasMarket := false
		for _, t := range topics {
			if strings.HasPrefix(t, "tm_nektar_") {
				hasMarket = true
				break
			}
		}
		if !hasMarket {
			return "", nil
		}
	}
	now := time.Now()
	// Validate carrier & payload against at least one matching market topic before charging!
	var lastErr error
	for _, topic := range topics {
		if !strings.HasPrefix(topic, "tm_nektar_") {
			continue
		}
		if isLive {
			_, p, vErr := bstack.VerifyMarketCarrier(tx, n.net, topic, now)
			if vErr == nil {
				return p.Kind, nil
			}
			lastErr = vErr
			continue
		}
		if len(tx.Inputs) > 0 {
			_, p, vErr := bstack.VerifyMarketCarrier(tx, n.net, topic, now)
			if vErr == nil {
				return p.Kind, nil
			}
			lastErr = vErr
		}
		for _, out := range tx.Outputs {
			p, ok, _ := bstack.ExtractNkt1Payload(out.LockingScript)
			if !ok {
				continue
			}
			if vErr := bstack.ValidateMarketPayload(p, n.net, topic, now); vErr == nil {
				return p.Kind, nil
			} else {
				lastErr = vErr
			}
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("hub: invalid market submission: %w", lastErr)
	}
	return "", nil
}
