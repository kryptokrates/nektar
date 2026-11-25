package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
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

// Submit processes a BRC-22 Atomic BEEF submission against requested topics, persists durably with a monotonic seq,
// notifies lookup services, and pushes numbered SSE events.
func (n *Node) Submit(ctx context.Context, rawBEEF []byte, topics []string) (overlay.Steak, error) {
	return n.submitInternal(ctx, rawBEEF, topics, false)
}

func (n *Node) submitInternal(ctx context.Context, rawBEEF []byte, topics []string, isFederation bool) (overlay.Steak, error) {
	if len(rawBEEF) == 0 {
		return nil, errors.New("hub: empty BEEF body")
	}
	if len(rawBEEF) > 4<<20 {
		return nil, errors.New("hub: BEEF exceeds 4 MiB limit")
	}
	isLive := n.net == "main" || n.net == "testnet"
	if isLive && n.tracker == nil {
		return nil, errors.New("hub: witnessed ChainTracker is required on live networks")
	}
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
		// Unfunded 0-input OP_FALSE OP_RETURN market carrier transactions allowed ONLY on memory/regtest.
		t0, tErr := transaction.NewTransactionFromBEEF(rawBEEF)
		if tErr != nil || t0 == nil || len(t0.Inputs) != 0 || len(t0.Outputs) == 0 || len(t0.Outputs) > 16 {
			return nil, fmt.Errorf("hub: parse carrier BEEF: %w", err)
		}
		tx = t0
	} else {
		if err == nil {
			err = errors.New("empty BEEF transaction")
		}
		return nil, fmt.Errorf("hub: parse BEEF: %w", err)
	}
	// Run overlay SPV verification against witnessed headers BEFORE admitting to any topic manager
	// (including all 6 mounted Lightweb modules which assume host-engine SPV already ran).
	if len(tx.Inputs) > 0 && n.tracker != nil {
		verdict, vErr := verify.Check(ctx, tx, n.tracker)
		if verdict != verify.Passed {
			if verdict == verify.Transport {
				return nil, fmt.Errorf("hub: tracker unavailable: %w", vErr)
			}
			return nil, fmt.Errorf("hub: SPV verification refused (%v): %w", verdict, vErr)
		}
	}
	txid := tx.TxID()
	beef := transaction.NewBeef()
	if _, err := beef.MergeTransaction(tx); err != nil {
		return nil, fmt.Errorf("hub: merge tx into BEEF: %w", err)
	}

	// Track previously admitted outpoints spent by tx.Inputs so TopicManagers receive accurate
	// previousCoins and LookupServices receive OutputSpent notifications.
	type spentPrev struct {
		outpointKey string
		outpoint    transaction.Outpoint
		topic       string
	}
	spentByTopic := make(map[string][]uint32)
	var spentList []spentPrev
	for idx, inp := range tx.Inputs {
		if inp.SourceTXID == nil {
			continue
		}
		op := transaction.Outpoint{Txid: *inp.SourceTXID, Index: inp.SourceTxOutIndex}
		prevTopic, alreadySpent, found, fErr := n.store.FindEventTopicByOutpoint(op.String())
		if fErr == nil && found && !alreadySpent {
			spentByTopic[prevTopic] = append(spentByTopic[prevTopic], uint32(idx))
			spentList = append(spentList, spentPrev{
				outpointKey: op.String(),
				outpoint:    op,
				topic:       prevTopic,
			})
		}
	}

	var blockHeight int64
	if tx.MerklePath != nil {
		blockHeight = int64(tx.MerklePath.BlockHeight)
	}
	subCtx := module.WithSubmission(ctx, rawBEEF)
	steak := make(overlay.Steak)
	shouldForwardUp := false
	var forwardTopics []string

	for _, topic := range topics {
		tm, ok := n.topics[topic]
		if !ok {
			continue
		}
		inst, err := tm.IdentifyAdmissibleOutputs(subCtx, beef, txid, spentByTopic[topic])
		if err != nil {
			return nil, fmt.Errorf("hub: topic %s: %w", topic, err)
		}
		var admitted []uint32
		for _, vout := range inst.OutputsToAdmit {
			if int(vout) >= len(tx.Outputs) {
				continue
			}
			out := tx.Outputs[vout]
			p, isNkt, _ := bstack.ExtractNkt1Payload(out.LockingScript)
			var pub string
			var domains []string
			var exp int64
			if isNkt {
				pub, domains, exp = extractMeta(p)
				if !isFederation && !n.allowPubRate(pub) {
					return nil, errors.New("hub: rate limit exceeded for pubkey")
				}
			}
			admitted = append(admitted, vout)
			op := transaction.Outpoint{Txid: *txid, Index: vout}
			var seq int64
			isNew := true
			nowMs := time.Now().UnixMilli()
			if isNkt {
				n.admitMu.Lock()
				seq, isNew, err = n.store.AppendEvent(StoredEvent{
					Outpoint:    op.String(),
					Topic:       topic,
					TxID:        txid.String(),
					Vout:        vout,
					Kind:        p.Kind,
					Pub:         pub,
					Domains:     domains,
					Payload:     p,
					BEEF:        rawBEEF,
					BlockHeight: blockHeight,
					Exp:         exp,
					AdmittedAt:  nowMs,
				})
				if err != nil {
					n.admitMu.Unlock()
					return nil, err
				}
				for _, ls := range n.lookups {
					_ = ls.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          ls.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
				}
				if n.marketLS != nil {
					n.marketLS.SetSeq(op.String(), seq)
				}
				if isNew {
					if p.Kind == bstack.KindWant || p.Kind == bstack.KindOffer || p.Kind == bstack.KindAnnounce {
						shouldForwardUp = true
						forwardTopics = append(forwardTopics, topic)
					}
					n.broadcastEvent(bstack.MarketEvent{
						Seq:        seq,
						Topic:      topic,
						TxID:       txid.String(),
						Vout:       vout,
						AdmittedAt: nowMs,
						Payload:    p,
						BEEF:       append([]byte(nil), rawBEEF...),
					}, domains)
				}
				n.admitMu.Unlock()
			} else {
				for _, ls := range n.lookups {
					_ = ls.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          ls.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
				}
			}
		}
		inst.OutputsToAdmit = admitted
		steak[topic] = &inst
	}

	// Spine-and-Leaf upstream push: when a Leaf Hub admits a new local market artifact, push
	// the verified BEEF UP to configured Spine(s) asynchronously (Leaf pays Spine toll if any).
	if !isFederation && shouldForwardUp && n.fees.Role == RoleLeaf {
		n.mu.RLock()
		spines := append([]string(nil), n.upstreamSpines...)
		subFn := n.upstreamSubmitter
		n.mu.RUnlock()
		if len(spines) > 0 && subFn != nil {
			beefCopy := append([]byte(nil), rawBEEF...)
			topicsCopy := append([]string(nil), forwardTopics...)
			go func() {
				for _, sp := range spines {
					upCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					_ = subFn(upCtx, sp, beefCopy, topicsCopy)
					cancel()
				}
			}()
		}
	}

	// Mark any previously admitted outpoints spent by this verified transaction and notify SpendListeners.
	for _, sp := range spentList {
		if _, found, mErr := n.store.MarkOutpointSpent(sp.outpointKey); mErr == nil && found {
			for _, ls := range n.lookups {
				if sl, ok := ls.(module.SpendListener); ok {
					_ = sl.OutputSpent(subCtx, &module.OutputSpent{
						Mode:         module.SpendTxid,
						Outpoint:     sp.outpoint,
						Topic:        sp.topic,
						SpendingTxid: txid,
					})
				}
			}
		}
	}
	return steak, nil
}

// Lookup executes a BRC-24 lookup query against a mounted lookup service.
func (n *Node) Lookup(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	if q == nil || q.Service == "" {
		return nil, errors.New("hub: lookup service name required")
	}
	ls, ok := n.lookups[q.Service]
	if !ok {
		return nil, fmt.Errorf("hub: unknown lookup service %q", q.Service)
	}
	return ls.Lookup(ctx, q)
}
