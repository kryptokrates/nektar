package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
)

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

	// Detect mineable kill-switch sweeps (1BSV Paper 011 §3.2 Fig. 9):
	// A final/mineable transaction spending a carrier's funding outpoint permanently retracts that
	// carrier across every conforming Hub and evicts it from ls_nektar.
	subCtx := module.WithSubmission(ctx, rawBEEF)
	isMineableTx := isMineableTransaction(tx)
	if isMineableTx && len(tx.Inputs) > 0 {
		for _, inp := range tx.Inputs {
			if inp == nil || inp.SourceTXID == nil {
				continue
			}
			fop := transaction.Outpoint{Txid: *inp.SourceTXID, Index: inp.SourceTxOutIndex}.String()
			retractedOP, retractedTopic, hadCarrier, sErr := n.store.MarkCarrierFundingSwept(fop, txid.String())
			if sErr == nil && hadCarrier && retractedOP != "" {
				if parsedOP, pErr := parseOutpointString(retractedOP); pErr == nil {
					for _, ls := range n.lookups {
						if sl, ok := ls.(module.SpendListener); ok {
							_ = sl.OutputSpent(subCtx, &module.OutputSpent{
								Mode:         module.SpendTxid,
								Outpoint:     parsedOP,
								Topic:        retractedTopic,
								SpendingTxid: txid,
							})
						}
					}
				}
			}
		}
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
	// Unmineable market carriers carry their funding tree's mined BUMP in ancestor transactions.
	// Record the maximum mined height across tx and its carried ancestors so HandleReorg evicts
	// carriers whose funding tree is reorged out.
	for _, btx := range beef.Transactions {
		if btx != nil && btx.Transaction != nil && btx.Transaction.MerklePath != nil {
			if h := int64(btx.Transaction.MerklePath.BlockHeight); h > blockHeight {
				blockHeight = h
			}
		}
	}
	steak := make(overlay.Steak)
	shouldForwardUp := false
	var forwardTopics []string
	var carrierFundingOP string
	if !isMineableTx && len(tx.Inputs) == 1 && tx.Inputs[0] != nil && tx.Inputs[0].SourceTXID != nil {
		carrierFundingOP = transaction.Outpoint{Txid: *tx.Inputs[0].SourceTXID, Index: tx.Inputs[0].SourceTxOutIndex}.String()
	}

	for _, topic := range topics {
		tm, ok := n.topics[topic]
		if !ok {
			continue
		}
		inst, err := tm.IdentifyAdmissibleOutputs(subCtx, beef, txid, spentByTopic[topic])
		if err != nil {
			// A pure kill-switch sweep tx has no carrier output to admit; treat ErrNotCarrier cleanly
			// when it already swept a funding outpoint.
			if isMineableTx && errors.Is(err, carrier.ErrNotCarrier) {
				steak[topic] = &overlay.AdmittanceInstructions{}
				continue
			}
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
			op := transaction.Outpoint{Txid: *txid, Index: vout}
			// Paper 011 §6 outpoint journal: one funding output admits at most one carrier forever,
			// and a swept funding output permanently rejects late/replayed carriers.
			if isNkt && carrierFundingOP != "" {
				if cErr := n.store.ClaimCarrierFundingOutpoint(carrierFundingOP, txid.String(), op.String(), topic); cErr != nil {
					return nil, fmt.Errorf("hub: topic %s: %w", topic, cErr)
				}
			}
			admitted = append(admitted, vout)
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
				// Paper 008 §5 Loop Guard: track exact (rawBEEF, topic) bytes. A proof-refreshed
				// BEEF carrying a newly mined BUMP has distinct bytes and passes cleanly to SSE,
				// Leaf->Spine upstream forwarding, and plane frames.
				unseenBytes := n.loopGuard.MarkIfUnseen(rawBEEF, topic)
				if isNew || unseenBytes {
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
					n.broadcastPlaneFrame(PlaneFrame{
						FlowSeq: uint64(seq),
						TopicID: TopicIDOf(topic),
						Topic:   topic,
						BEEF:    append([]byte(nil), rawBEEF...),
					})
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

	// Spine-and-Leaf upstream push: when a Leaf Hub admits a new local market artifact (and
	// propagation is NOT switched off for overlay-bridge / unicast mode), push the verified BEEF
	// ONCE UP to configured Spine(s) asynchronously.
	if !isFederation && !n.propagationOff && n.transportMode != TransportPlane && n.transportMode != TransportUnicast && shouldForwardUp && n.fees.Role == RoleLeaf {
		n.mu.RLock()
		spines := append([]string(nil), n.upstreamSpines...)
		subFn := n.upstreamSubmitter
		n.mu.RUnlock()
		if len(spines) > 0 && subFn != nil {
			beefCopy := append([]byte(nil), rawBEEF...)
			topicsCopy := append([]string(nil), forwardTopics...)
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				if n.upstreamSem != nil {
					select {
					case n.upstreamSem <- struct{}{}:
						defer func() { <-n.upstreamSem }()
					case <-n.stopCh:
						return
					}
				}
				for _, sp := range spines {
					for attempt := 0; attempt < 2; attempt++ {
						upCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						doneWatch := make(chan struct{})
						go func() {
							select {
							case <-n.stopCh:
								cancel()
							case <-doneWatch:
							}
						}()
						uErr := subFn(upCtx, sp, beefCopy, topicsCopy)
						close(doneWatch)
						cancel()
						if uErr == nil {
							break
						}
						lower := strings.ToLower(uErr.Error())
						if attempt == 0 && (strings.Contains(lower, "deadline") || strings.Contains(lower, "busy") || strings.Contains(lower, "locked") || strings.Contains(lower, "429")) {
							select {
							case <-n.stopCh:
								return
							case <-time.After(1 * time.Second):
							}
							continue
						}
						log.Printf("hub: upstream push to %s (%v) failed: %v", sp, topicsCopy, uErr)
						break
					}
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

// isMineableTransaction reports whether tx could be included in a block before 2100-01-01
// (inverse of bcommon/carrier's unmineable rule: LockTime < carrier.LockTime OR all inputs final).
func isMineableTransaction(tx *transaction.Transaction) bool {
	if tx == nil || len(tx.Inputs) == 0 {
		return false
	}
	if tx.LockTime < carrier.LockTime {
		return true
	}
	for _, inp := range tx.Inputs {
		if inp == nil || inp.SequenceNumber != transaction.MaxTxInSequenceNum {
			return false
		}
	}
	return true
}

func parseOutpointString(s string) (transaction.Outpoint, error) {
	txHex, idxStr, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		txHex, idxStr, ok = strings.Cut(strings.TrimSpace(s), ":")
	}
	if !ok {
		return transaction.Outpoint{}, fmt.Errorf("invalid outpoint %q", s)
	}
	h, err := chainhash.NewHashFromHex(txHex)
	if err != nil || h == nil {
		return transaction.Outpoint{}, fmt.Errorf("invalid outpoint txid %q", txHex)
	}
	var vout uint32
	if _, err := fmt.Sscanf(idxStr, "%d", &vout); err != nil {
		return transaction.Outpoint{}, fmt.Errorf("invalid outpoint vout %q", idxStr)
	}
	return transaction.Outpoint{Txid: *h, Index: vout}, nil
}
