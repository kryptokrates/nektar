package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// Submit processes a BRC-22 Atomic BEEF submission against requested topics, persists durably with a monotonic seq,
// notifies lookup services, pushes numbered SSE events, and fans out over BRC-223 topic interest lanes.
func (n *Node) Submit(ctx context.Context, rawBEEF []byte, topics []string) (overlay.Steak, error) {
	return n.submitInternal(ctx, rawBEEF, topics, false)
}

func (n *Node) laneLock(topic string) *sync.Mutex {
	if len(topic) > 0 {
		last := topic[len(topic)-1]
		if last >= '0' && last <= '9' {
			return &n.laneAdmitMu[last-'0']
		}
		if last >= 'a' && last <= 'f' {
			return &n.laneAdmitMu[10+(last-'a')]
		}
	}
	return &n.admitMu
}

func (n *Node) submitInternal(ctx context.Context, rawBEEF []byte, topics []string, isFederation bool) (overlay.Steak, error) {
	if len(rawBEEF) == 0 {
		return nil, fmt.Errorf("%w: empty BEEF body", ErrHubRefused)
	}
	if len(rawBEEF) > 4<<20 {
		return nil, fmt.Errorf("%w: BEEF exceeds 4 MiB limit", ErrHubRefused)
	}
	isLive := n.net == "main" || n.net == "testnet"
	if isLive && n.tracker == nil {
		return nil, fmt.Errorf("%w: witnessed ChainTracker is required on live networks", ErrHubTransient)
	}
	hasMarketTopic := false
	hasNonMarketKnownTopic := false
	for _, t := range topics {
		if strings.HasPrefix(t, "tm_nektar_") {
			hasMarketTopic = true
		} else if _, known := n.resolveTopicManager(t); known {
			hasNonMarketKnownTopic = true
		}
	}
	allMarketTopics := hasMarketTopic && !hasNonMarketKnownTopic
	var tx *transaction.Transaction
	_, parsedTx, _, err := guard.ParseBEEF(rawBEEF, guard.DefaultBound)
	if err == nil && parsedTx != nil {
		tx = parsedTx
	} else if (!isLive || os.Getenv("NEKTAR_OFFCHAIN_ADS") == "1") && allMarketTopics && err != nil && strings.Contains(err.Error(), "no inputs") && len(rawBEEF) <= 64<<10 {
		// Unfunded 0-input OP_FALSE OP_RETURN signed market envelopes (memory/regtest or NEKTAR_OFFCHAIN_ADS=1).
		t0, tErr := transaction.NewTransactionFromBEEF(rawBEEF)
		if tErr != nil || t0 == nil || len(t0.Inputs) != 0 || len(t0.Outputs) == 0 || len(t0.Outputs) > 16 {
			return nil, fmt.Errorf("%w: parse carrier BEEF: %w", ErrHubRefused, err)
		}
		tx = t0
	} else {
		if err == nil {
			err = errors.New("empty BEEF transaction")
		}
		return nil, fmt.Errorf("%w: parse BEEF: %w", ErrHubRefused, err)
	}
	// Run overlay-bridge SPV verification against witnessed headers BEFORE admitting to any topic manager
	// (including all mounted Lightweb modules which assume host-engine SPV already ran).
	if err := n.verifyBEEFWithSPV(ctx, rawBEEF, tx); err != nil {
		return nil, err
	}
	txid := tx.TxID()
	beef := transaction.NewBeef()
	if _, err := beef.MergeTransaction(tx); err != nil {
		return nil, fmt.Errorf("%w: merge tx into BEEF: %w", ErrHubRefused, err)
	}

	n.mu.RLock()
	lookupsSnap := make([]module.LookupService, 0, len(n.lookups))
	for _, ls := range n.lookups {
		lookupsSnap = append(lookupsSnap, ls)
	}
	n.mu.RUnlock()

	// Detect mineable kill-switch sweeps (1BSV Paper 011 §3.2 Fig. 9):
	// A final/mineable transaction spending a carrier's funding outpoint permanently retracts that
	// carrier across every conforming Hub and evicts it from ls_nektar.
	subCtx := module.WithSubmission(ctx, rawBEEF)
	isMineableTx := isMineableTransaction(tx)
	if isMineableTx && len(tx.Inputs) > 0 {
		for vin, inp := range tx.Inputs {
			if inp == nil || inp.SourceTXID == nil {
				continue
			}
			fop := transaction.Outpoint{Txid: *inp.SourceTXID, Index: inp.SourceTxOutIndex}.String()
			retractedOP, retractedTopic, hadCarrier, sErr := n.store.MarkCarrierFundingSwept(fop, txid.String())
			if sErr == nil && hadCarrier && retractedOP != "" {
				if parsedOP, pErr := parseOutpointString(retractedOP); pErr == nil {
					for _, ls := range lookupsSnap {
						if sl, ok := ls.(module.SpendListener); ok {
							mode := ls.SpendNotificationMode()
							if mode == "" {
								mode = module.SpendTxid
							}
							_ = sl.OutputSpent(subCtx, &module.OutputSpent{
								Mode:               mode,
								Outpoint:           parsedOP,
								Topic:              retractedTopic,
								SpendingTxid:       txid,
								InputIndex:         uint32(vin),
								UnlockingScript:    inp.UnlockingScript,
								SequenceNumber:     inp.SequenceNumber,
								SpendingAtomicBEEF: rawBEEF,
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
		inputIndex  uint32
		inp         *transaction.TransactionInput
		isMod       bool
	}
	spentByTopic := make(map[string][]uint32)
	var spentList []spentPrev
	for idx, inp := range tx.Inputs {
		if inp == nil || inp.SourceTXID == nil {
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
				inputIndex:  uint32(idx),
				inp:         inp,
			})
			continue
		}
		if modTopic, modSpent, modFound := n.findModuleOutpointTopic(ctx, op); modFound && !modSpent {
			spentByTopic[modTopic] = append(spentByTopic[modTopic], uint32(idx))
			spentList = append(spentList, spentPrev{
				outpointKey: op.String(),
				outpoint:    op,
				topic:       modTopic,
				inputIndex:  uint32(idx),
				inp:         inp,
				isMod:       true,
			})
		}
	}

	var blockHeight int64
	if tx.MerklePath != nil {
		blockHeight = int64(tx.MerklePath.BlockHeight)
	}
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
	var fanoutTopics []string
	var carrierFundingOP string
	if !isMineableTx && len(tx.Inputs) == 1 && tx.Inputs[0] != nil && tx.Inputs[0].SourceTXID != nil {
		carrierFundingOP = transaction.Outpoint{Txid: *tx.Inputs[0].SourceTXID, Index: tx.Inputs[0].SourceTxOutIndex}.String()
	}

	// Within a single HTTP /submit call carrying multiple co-located topics (e.g. [laneTopic, domTopic]),
	// track already-verified and already-admitted nkt1 vouts so we verify ECDSA once, persist once,
	// and broadcast SSE once while still populating overlay.Steak, BRC-223 fabric, BRC-149 plane, and
	// upstream Spine forwarding for every co-located topic!
	verifiedNktVouts := make(map[uint32]bstack.MarketPayload, 2)
	admittedNktVouts := make(map[uint32]bool, 2)
	admittedNktSeq := make(map[uint32]int64, 2)

	for _, topic := range topics {
		tm, ok := n.resolveTopicManager(topic)
		if !ok {
			continue
		}
		var inst overlay.AdmittanceInstructions
		if len(verifiedNktVouts) > 0 && strings.HasPrefix(topic, "tm_nektar_") {
			for vout, vp := range verifiedNktVouts {
				if bstack.TopicMatchesPayload(n.net, topic, vp) {
					inst.OutputsToAdmit = append(inst.OutputsToAdmit, vout)
				}
			}
		} else {
			tCtx := subCtx
			if pc, ok := tm.(interface {
				Precheck(context.Context) context.Context
			}); ok {
				tCtx = pc.Precheck(subCtx)
			}
			var tmErr error
			inst, tmErr = tm.IdentifyAdmissibleOutputs(tCtx, beef, txid, spentByTopic[topic])
			if tmErr != nil {
				if isMineableTx && errors.Is(tmErr, carrier.ErrNotCarrier) {
					steak[topic] = &overlay.AdmittanceInstructions{}
					continue
				}
				return nil, fmt.Errorf("%w: hub: topic %s: %w", ErrHubRefused, topic, tmErr)
			}
		}
		var admitted []uint32
		for _, vout := range inst.OutputsToAdmit {
			if int(vout) >= len(tx.Outputs) {
				continue
			}
			if admittedNktVouts[vout] {
				admitted = append(admitted, vout)
				if n.loopGuard.MarkIfUnseen(rawBEEF, topic) {
					fanoutTopics = append(fanoutTopics, topic)
					vp := verifiedNktVouts[vout]
					if vp.Kind == bstack.KindWant || vp.Kind == bstack.KindOffer || vp.Kind == bstack.KindAnnounce {
						shouldForwardUp = true
						forwardTopics = append(forwardTopics, topic)
					}
					if prevSeq := admittedNktSeq[vout]; prevSeq > 0 {
						n.mu.RLock()
						hasPlane := len(n.planeSubs) > 0
						n.mu.RUnlock()
						if hasPlane {
							n.broadcastPlaneFrame(PlaneFrame{
								FlowSeq: uint64(prevSeq),
								TopicID: TopicIDOf(topic),
								Topic:   topic,
								BEEF:    append([]byte(nil), rawBEEF...),
							})
						}
					}
				}
				if bus := n.EventBus(); bus != nil {
					op := transaction.Outpoint{Txid: *txid, Index: vout}
					bus.Publish("hub:admit:"+topic, "admit", op.String())
				}
				continue
			}
			out := tx.Outputs[vout]
			p, isNkt, _ := bstack.ExtractNkt1Payload(out.LockingScript)
			if isNkt {
				verifiedNktVouts[vout] = p
			}
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
			admitted = append(admitted, vout)
			var seq int64
			isNew := true
			nowMs := time.Now().UnixMilli()
			if isNkt {
				admittedNktVouts[vout] = true
				// When Pebble fastEventStore is enabled, AppendEventWithCarrierClaim uses 64-shard
				// outpoint stripe locking + atomic seq, marketLS has its own lock, and SSE deduplicates
				// against backlogWatermark — so no per-lane mutex is needed (zero cross-request stall!).
				var laneMu *sync.Mutex
				if n.store.fastStore() == nil {
					laneMu = n.laneLock(topic)
					laneMu.Lock()
				}
				// ARCH-F11 + PERF-5: claim carrier funding outpoint AND append event inside ONE SQLite tx
				// (or lock-free Pebble WAL group-commit batch when fastStore is enabled)!
				seq, isNew, err = n.store.AppendEventWithCarrierClaim(StoredEvent{
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
				}, carrierFundingOP)
				if err != nil {
					if laneMu != nil {
						laneMu.Unlock()
					}
					if errors.Is(err, ErrCarrierRetracted) || errors.Is(err, ErrCarrierFundingReused) {
						return nil, fmt.Errorf("%w: hub: topic %s: %w", ErrHubRefused, topic, err)
					}
					return nil, err
				}
				admittedNktSeq[vout] = seq
				if n.oeStore != nil {
					n.recordModuleOutput(ctx, topic, txid, vout, beef, rawBEEF, inst.AncillaryTxids)
				}
				if n.marketLS != nil {
					_ = n.marketLS.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          n.marketLS.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
					n.marketLS.SetSeq(op.String(), seq)
				} else {
					for _, ls := range lookupsSnap {
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
				unseenBytes := n.loopGuard.MarkIfUnseen(rawBEEF, topic)
				if isNew || unseenBytes {
					fanoutTopics = append(fanoutTopics, topic)
					if p.Kind == bstack.KindWant || p.Kind == bstack.KindOffer || p.Kind == bstack.KindAnnounce {
						shouldForwardUp = true
						forwardTopics = append(forwardTopics, topic)
					}
					var buyerPub string
					if p.Kind == bstack.KindOffer {
						if so, dErr := bstack.DecodeOfferPayload(p.Body); dErr == nil {
							buyerPub = so.Offer.BuyerPub
						}
					}
					n.broadcastEvent(bstack.MarketEvent{
						Seq:        seq,
						Topic:      topic,
						TxID:       txid.String(),
						Vout:       vout,
						AdmittedAt: nowMs,
						Payload:    p,
						BEEF:       rawBEEF,
						BuyerPub:   buyerPub,
					}, domains)
					n.mu.RLock()
					hasPlane := len(n.planeSubs) > 0
					n.mu.RUnlock()
					if hasPlane {
						n.broadcastPlaneFrame(PlaneFrame{
							FlowSeq: uint64(seq),
							TopicID: TopicIDOf(topic),
							Topic:   topic,
							BEEF:    append([]byte(nil), rawBEEF...),
						})
					}
				}
				if laneMu != nil {
					laneMu.Unlock()
				}
			} else {
				n.recordModuleOutput(ctx, topic, txid, vout, beef, rawBEEF, inst.AncillaryTxids)
				modSeq, modNew, _ := n.store.AppendEvent(StoredEvent{
					Outpoint:    op.String(),
					Topic:       topic,
					TxID:        txid.String(),
					Vout:        vout,
					Kind:        bstack.ArtifactKind("module"),
					BEEF:        rawBEEF,
					BlockHeight: blockHeight,
					AdmittedAt:  nowMs,
				})
				for _, ls := range lookupsSnap {
					_ = ls.OutputAdmittedByTopic(subCtx, &module.OutputAdmittedByTopic{
						Mode:          ls.AdmissionMode(),
						Outpoint:      op,
						Topic:         topic,
						Satoshis:      out.Satoshis,
						LockingScript: out.LockingScript,
						AtomicBEEF:    rawBEEF,
					})
				}
				unseenMod := n.loopGuard.MarkIfUnseen(rawBEEF, topic)
				if modNew || unseenMod {
					fanoutTopics = append(fanoutTopics, topic)
					shouldForwardUp = true
					forwardTopics = append(forwardTopics, topic)
					if modSeq > 0 {
						n.mu.RLock()
						hasPlane := len(n.planeSubs) > 0
						n.mu.RUnlock()
						if hasPlane {
							n.broadcastPlaneFrame(PlaneFrame{
								FlowSeq: uint64(modSeq),
								TopicID: TopicIDOf(topic),
								Topic:   topic,
								BEEF:    append([]byte(nil), rawBEEF...),
							})
						}
					}
				}
			}
			if bus := n.EventBus(); bus != nil {
				bus.Publish("hub:admit:"+topic, "admit", op.String())
			}
		}
		inst.OutputsToAdmit = admitted
		steak[topic] = &inst
	}

	// Fan out admitted BEEFs over BRC-223 topic interest distributor (O(1) non-blocking copy per interested subscriber).
	if n.fabricSrv != nil && len(fanoutTopics) > 0 {
		n.fabricSrv.Fanout(fanoutTopics, rawBEEF, netip.Addr{})
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
			acquired := true
			if n.upstreamSem != nil {
				select {
				case n.upstreamSem <- struct{}{}:
				case <-n.stopCh:
					acquired = false
				case <-time.After(250 * time.Millisecond):
					acquired = false
				}
			}
			if acquired {
				beefCopy := append([]byte(nil), rawBEEF...)
				topicsCopy := append([]string(nil), forwardTopics...)
				n.wg.Add(1)
				go func() {
					defer n.wg.Done()
					if n.upstreamSem != nil {
						defer func() { <-n.upstreamSem }()
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
	}

	// Mark any previously admitted outpoints spent by this verified transaction and notify SpendListeners.
	for _, sp := range spentList {
		notifySpend := false
		if sp.isMod {
			n.markModuleOutpointSpent(ctx, sp.outpoint, sp.topic)
			notifySpend = true
		} else if _, found, mErr := n.store.MarkOutpointSpent(sp.outpointKey); mErr == nil && found {
			notifySpend = true
		}
		if notifySpend {
			for _, ls := range lookupsSnap {
				if sl, ok := ls.(module.SpendListener); ok {
					mode := ls.SpendNotificationMode()
					if mode == "" || mode == module.SpendNone {
						continue
					}
					_ = sl.OutputSpent(subCtx, &module.OutputSpent{
						Mode:               mode,
						Outpoint:           sp.outpoint,
						Topic:              sp.topic,
						SpendingTxid:       txid,
						InputIndex:         sp.inputIndex,
						UnlockingScript:    sp.inp.UnlockingScript,
						SequenceNumber:     sp.inp.SequenceNumber,
						SpendingAtomicBEEF: rawBEEF,
					})
				}
			}
		}
	}
	return steak, nil
}

// Lookup executes a BRC-24 lookup query against a mounted lookup service, hydrating formula answers
// into AtomicBEEF output-lists when needed.
func (n *Node) Lookup(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	if q == nil || q.Service == "" {
		return nil, errors.New("hub: lookup service name required")
	}
	n.mu.RLock()
	ls, ok := n.lookups[q.Service]
	n.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("hub: unknown lookup service %q", q.Service)
	}
	ans, err := ls.Lookup(ctx, q)
	if err != nil {
		return nil, err
	}
	return n.hydrateLookupFormulas(ctx, ans), nil
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
