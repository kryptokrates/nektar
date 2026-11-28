package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"

	"github.com/kryptokrates/nektar/internal/bstack"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (n *Node) handleHealth(w http.ResponseWriter, _ *http.Request) {
	bounties, _ := n.store.ListBounties(true)
	resp := map[string]any{
		"ok":                  true,
		"network":             n.net,
		"hubRole":             n.fees.Role,
		"transportMode":       n.transportMode,
		"propagationOff":      n.propagationOff,
		"payeeIdentityKey":    n.PayeeIdentityKey(),
		"fees":                n.fees.AdvertSummary(),
		"topics":              n.Topics(),
		"lookups":             n.Lookups(),
		"activeBounties":      len(bounties),
		"defaultBboxOffice":   n.DefaultBboxOffice(),
		"defaultBstoreBucket": n.DefaultBstoreBucket(),
		"poisonFramesSkipped": n.PoisonFramesSkipped(),
	}
	if n.fabricSrv != nil {
		resp["brc223UDPAddr"] = n.fabricSrv.UDPAddr().String()
	}
	writeJSON(w, http.StatusOK, resp)
}

func formatOverlayMeta(name, defaultDesc string, md *overlay.MetaData) map[string]any {
	meta := map[string]any{
		"name":             name,
		"shortDescription": defaultDesc,
		"version":          "0.1.0",
	}
	if md != nil {
		if md.Name != "" {
			meta["name"] = md.Name
		}
		if md.Description != "" {
			meta["shortDescription"] = md.Description
		}
		if md.Version != "" {
			meta["version"] = md.Version
		}
		if md.Icon != "" {
			meta["iconURL"] = md.Icon
		}
		if md.InfoUrl != "" {
			meta["informationURL"] = md.InfoUrl
		}
	}
	return meta
}

func writeJSONWithETag(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleListTopicManagers implements the standard BRC-22 engine metadata route consumed by
// lightwebinc/overlay-bridge (`-topics-from engine`), including ETag / 304 Not Modified support.
func (n *Node) handleListTopicManagers(w http.ResponseWriter, r *http.Request) {
	n.mu.RLock()
	out := make(map[string]map[string]any, len(n.topics))
	for name, tm := range n.topics {
		out[name] = formatOverlayMeta(name, "Nektar v0.1 overlay topic manager", tm.GetMetaData())
	}
	n.mu.RUnlock()
	writeJSONWithETag(w, r, out)
}

// handleListLookupServiceProviders implements the standard BRC-24 engine metadata route.
func (n *Node) handleListLookupServiceProviders(w http.ResponseWriter, r *http.Request) {
	n.mu.RLock()
	out := make(map[string]map[string]any, len(n.lookups))
	for name, ls := range n.lookups {
		out[name] = formatOverlayMeta(name, "Nektar v0.1 overlay lookup service", ls.GetMetaData())
	}
	n.mu.RUnlock()
	writeJSONWithETag(w, r, out)
}

func (n *Node) handleTerms(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.Terms())
}

func (n *Node) handleEarnings(w http.ResponseWriter, _ *http.Request) {
	sum, err := n.store.FeeEarningsSummary(time.Now().Add(-24 * time.Hour).Unix())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (n *Node) handleSubmit(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var topics []string
	if hdr := r.Header.Get("X-Topics"); hdr != "" {
		if err := json.Unmarshal([]byte(hdr), &topics); err != nil {
			for _, t := range strings.Split(hdr, ",") {
				if t = strings.TrimSpace(t); t != "" {
					topics = append(topics, t)
				}
			}
		}
	}
	if len(topics) == 0 {
		topics = n.Topics()
	}
	// Valve 1 (Validate-before-charge): classify & validate market payload before asking for or claiming payment!
	collectedTxID, ok := n.enforceSubmitFee(w, r, "hub_submit", raw, topics)
	if !ok {
		return
	}
	// Paper 008 §12.1: honor explicit plane/historical submission mode so deliveries arriving
	// from overlay-bridge or a BRC-149 relay never re-propagate.
	submitMode := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Nektar-Submit-Mode")))
	suppressProp := submitMode == "plane" || submitMode == "historical"
	steak, err := n.submitInternal(r.Context(), raw, topics, suppressProp)
	if err != nil {
		if collectedTxID != "" {
			_ = n.store.ReleaseFeeClaim(collectedTxID)
		}
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "rate limit") {
			code = http.StatusTooManyRequests
		} else if errors.Is(err, ErrHubTransient) || strings.Contains(err.Error(), "tracker unavailable") {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-BSV-Engine-Format")), "steak") {
		writeJSON(w, http.StatusOK, map[string]overlay.Steak{"STEAK": steak})
		return
	}
	writeJSON(w, http.StatusOK, steak)
}

func (n *Node) handleLookup(w http.ResponseWriter, r *http.Request) {
	var q lookup.LookupQuestion
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Valve 4 (Lightweb termsroute priced lookup classes): live discovery free; history/export priced.
	if len(n.fees.LookupPrices) > 0 {
		cls := classifyLookupClass(&q)
		if price := n.fees.LookupPrices[cls]; price > 0 {
			action := ActionLookupPrefix + cls
			out := n.verifyAndCollectFee(r.Context(), action, cls, price, r.Header.Get("X-BSV-Payment"))
			if out.HTTPStatus != http.StatusOK {
				n.writeFeeOutcome(w, action, price, out)
				return
			}
		}
	}
	ans, err := n.Lookup(r.Context(), &q)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ans)
}

// BboxPlaneTopic returns the deterministic BRC-149 plane relay topic for a normalized bbox office.
func BboxPlaneTopic(office string) string {
	return "tm_nektar_bbox_" + bstack.NormalizeBboxOffice(office)
}

func bboxAudMatchesHubOrOffice(aud, hostHeader, office string) bool {
	aud = strings.ToLower(strings.TrimSpace(aud))
	if aud == "" {
		return false
	}
	if strings.EqualFold(aud, "office:"+bstack.NormalizeBboxOffice(office)) {
		return true
	}
	if hostHeader == "" {
		return true
	}
	return strings.EqualFold(aud, strings.TrimSpace(hostHeader))
}

func (n *Node) admitRelayedBboxPlaneFrame(raw []byte) error {
	var msg BboxMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ErrHubRefused
	}
	if msg.RecipientPub == "" || msg.SenderPub == "" || msg.DealID == "" || msg.Kind == "" || strings.TrimSpace(msg.Nonce) == "" || msg.Sig == "" {
		return ErrHubRefused
	}
	now := time.Now().Unix()
	if msg.TS < now-bboxSkewSeconds || msg.TS > now+bboxSkewSeconds {
		return nil // expired frame; skip cleanly withoutpoison-stalling
	}
	if !strings.EqualFold(msg.Aud, "office:"+bstack.NormalizeBboxOffice(msg.Office)) {
		return ErrHubRefused
	}
	if err := bstack.VerifyArtifact(msg.SenderPub, msg.Sig, msg.Signable()); err != nil {
		return ErrHubRefused
	}
	if !n.claimBboxNonce("send:"+msg.SenderPub, msg.Nonce, now) {
		return nil // idempotent duplicate
	}
	seq, err := n.store.SaveBboxMessage(msg)
	if err != nil {
		return err
	}
	planeTopic := BboxPlaneTopic(msg.Office)
	rawCopy := append([]byte(nil), raw...)
	modSeq, _, _ := n.store.AppendEvent(StoredEvent{
		Outpoint:   "bbox:" + strings.ToLower(msg.SenderPub) + ":" + msg.Nonce,
		Topic:      planeTopic,
		Kind:       bstack.ArtifactKind("module"),
		Pub:        msg.RecipientPub,
		BEEF:       rawCopy,
		Exp:        (now + 600) * 1000,
		AdmittedAt: time.Now().UnixMilli(),
	})
	if modSeq > 0 {
		n.broadcastPlaneFrame(PlaneFrame{
			FlowSeq: uint64(modSeq),
			TopicID: TopicIDOf(planeTopic),
			Topic:   planeTopic,
			BEEF:    rawCopy,
		})
	}
	if bus := n.EventBus(); bus != nil {
		bus.Publish("hub:bbox:"+strings.ToLower(strings.TrimSpace(msg.RecipientPub)), "bbox", seq)
	}
	n.broadcastBboxWake(msg.RecipientPub)
	return nil
}

func (n *Node) handleBboxSend(w http.ResponseWriter, r *http.Request) {
	var msg BboxMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&msg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if msg.RecipientPub == "" || msg.SenderPub == "" || msg.DealID == "" || msg.Kind == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recipientPub, senderPub, dealId, and kind are required"})
		return
	}
	now := time.Now().Unix()
	if msg.TS < now-bboxSkewSeconds || msg.TS > now+bboxSkewSeconds {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired or missing bbox ts"})
		return
	}
	if strings.TrimSpace(msg.Nonce) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox nonce required"})
		return
	}
	if !bboxAudMatchesHubOrOffice(msg.Aud, r.Host, msg.Office) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox aud does not match hub host"})
		return
	}
	if msg.Sig == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bbox message signature required"})
		return
	}
	if err := bstack.VerifyArtifact(msg.SenderPub, msg.Sig, msg.Signable()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox sender signature"})
		return
	}
	// Rate-limit BEFORE claiming global bboxNonce slot or charging fee so 1 client cannot DoS Hub-wide bbox.
	if !n.allowPubRate(msg.SenderPub) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	// Check replayed nonce BEFORE charging Valve 3 fee so replayed messages never consume a payment,
	// while preserving unpaid 402 challenge retries (PostJSONWithHub402 resends the same body + nonce with X-BSV-Payment).
	if n.hasBboxNonce("send:"+msg.SenderPub, msg.Nonce) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "replayed bbox message nonce"})
		return
	}
	// Valve 3: enforce unbypassable NAT relay toll AFTER validating sender signature & replay check.
	var collectedTxID string
	if n.fees.BboxSendSats > 0 {
		out := n.verifyAndCollectFee(r.Context(), ActionBboxSend, msg.Kind, n.fees.BboxSendSats, r.Header.Get("X-BSV-Payment"))
		if out.HTTPStatus != http.StatusOK {
			n.writeFeeOutcome(w, ActionBboxSend, n.fees.BboxSendSats, out)
			return
		}
		collectedTxID = out.TxID
	}
	if !n.claimBboxNonce("send:"+msg.SenderPub, msg.Nonce, now) {
		if collectedTxID != "" {
			_ = n.store.ReleaseFeeClaim(collectedTxID)
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": "replayed bbox message nonce"})
		return
	}
	seq, err := n.store.SaveBboxMessage(msg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	isOfficeBound := strings.EqualFold(strings.TrimSpace(msg.Aud), "office:"+bstack.NormalizeBboxOffice(msg.Office))
	if isOfficeBound {
		if rawMsg, mErr := json.Marshal(msg); mErr == nil {
			planeTopic := BboxPlaneTopic(msg.Office)
			n.loopGuard.MarkIfUnseen(rawMsg, planeTopic)
			modSeq, _, _ := n.store.AppendEvent(StoredEvent{
				Outpoint:   "bbox:" + strings.ToLower(msg.SenderPub) + ":" + msg.Nonce,
				Topic:      planeTopic,
				Kind:       bstack.ArtifactKind("module"),
				Pub:        msg.RecipientPub,
				BEEF:       rawMsg,
				Exp:        (now + 600) * 1000,
				AdmittedAt: time.Now().UnixMilli(),
			})
			if modSeq > 0 {
				n.broadcastPlaneFrame(PlaneFrame{
					FlowSeq: uint64(modSeq),
					TopicID: TopicIDOf(planeTopic),
					Topic:   planeTopic,
					BEEF:    rawMsg,
				})
			}
			// Leaf -> Spine free upstream push for office-bound NAT settlement envelopes.
			if r.Header.Get("X-Nektar-Bbox-Relayed") == "" && !n.propagationOff && n.fees.Role != RoleSpine {
				n.mu.RLock()
				spines := append([]string(nil), n.upstreamSpines...)
				n.mu.RUnlock()
				if len(spines) > 0 {
					msgCopy := append([]byte(nil), rawMsg...)
					n.wg.Add(1)
					go func() {
						defer n.wg.Done()
						for _, sp := range spines {
							uCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							uReq, rErr := http.NewRequestWithContext(uCtx, http.MethodPost, strings.TrimRight(sp, "/")+"/bbox/send", bytes.NewReader(msgCopy))
							if rErr == nil {
								uReq.Header.Set("Content-Type", "application/json")
								uReq.Header.Set("X-Nektar-Bbox-Relayed", "1")
								if uResp, dErr := n.httpClient.Do(uReq); dErr == nil && uResp != nil {
									uResp.Body.Close()
								}
							}
							cancel()
						}
					}()
				}
			}
		}
	}
	if bus := n.EventBus(); bus != nil {
		bus.Publish("hub:bbox:"+strings.ToLower(strings.TrimSpace(msg.RecipientPub)), "bbox", seq)
	}
	n.broadcastBboxWake(msg.RecipientPub)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": seq})
}

func (n *Node) handleBboxInbox(w http.ResponseWriter, r *http.Request) {
	pub := strings.TrimSpace(r.URL.Query().Get("pub"))
	if pub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub query parameter required"})
		return
	}
	hdrPub := strings.TrimSpace(r.Header.Get("X-Nektar-Pub"))
	hdrTs := strings.TrimSpace(r.Header.Get("X-Nektar-Ts"))
	hdrNonce := strings.TrimSpace(r.Header.Get("X-Nektar-Nonce"))
	hdrSig := strings.TrimSpace(r.Header.Get("X-Nektar-Sig"))
	if hdrPub != pub || hdrTs == "" || hdrNonce == "" || hdrSig == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signed X-Nektar-Pub / X-Nektar-Ts / X-Nektar-Nonce / X-Nektar-Sig headers required for bbox inbox"})
		return
	}
	ts, tsErr := strconv.ParseInt(hdrTs, 10, 64)
	now := time.Now().Unix()
	if tsErr != nil || ts < now-bboxSkewSeconds || ts > now+bboxSkewSeconds {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired or invalid X-Nektar-Ts"})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	aud := strings.ToLower(strings.TrimSpace(r.Host))
	authPayload := bstack.BboxInboxAuth{
		Pub:   pub,
		TS:    ts,
		Nonce: hdrNonce,
		Aud:   aud,
		Since: since,
	}
	if err := bstack.VerifyArtifact(pub, hdrSig, authPayload); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bbox inbox signature"})
		return
	}
	if !n.allowPubRate(pub) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	if !n.claimBboxNonce("inbox:"+pub, hdrNonce, now) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "replayed bbox inbox nonce"})
		return
	}
	msgs, err := n.store.ListBboxMessages(pub, since, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (n *Node) handleBountiesList(w http.ResponseWriter, r *http.Request) {
	bounties, err := n.store.ListBounties(true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for i := range bounties {
		bounties[i].RewardSats = EffectiveBountyReward(bounties[i])
	}
	var standingBounties []bstack.WantAd
	if n.marketLS != nil {
		qRaw, _ := json.Marshal(bstack.MarketQuery{Kind: bstack.KindWant, Limit: 100})
		if ans, lErr := n.marketLS.Lookup(r.Context(), &lookup.LookupQuestion{Service: bstack.LookupService, Query: qRaw}); lErr == nil && ans != nil {
			if evs, ok := ans.Result.([]bstack.MarketEvent); ok {
				nowMs := time.Now().UnixMilli()
				seenWant := make(map[string]bool, len(evs))
				for _, ev := range evs {
					sw, dErr := bstack.DecodeWantPayload(ev.Payload.Body)
					if dErr != nil || sw.Want.WantID == "" || sw.Want.MaxSats == 0 {
						continue
					}
					if sw.Want.Exp > 0 && sw.Want.Exp <= nowMs {
						continue
					}
					if seenWant[sw.Want.WantID] {
						continue
					}
					seenWant[sw.Want.WantID] = true
					standingBounties = append(standingBounties, sw.Want)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bounties":         bounties,
		"standingBounties": standingBounties,
	})
}

func (n *Node) handleBountiesSubmit(w http.ResponseWriter, r *http.Request) {
	var sub BountySubmission
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if sub.HDP == "" || sub.Text == "" || sub.SellerPub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hdp, text, and sellerPub are required"})
		return
	}
	// Canonicalize HDP to 64 lowercase hex chars BEFORE deduplicating in SQLite!
	hdpRaw, hErr := hex.DecodeString(strings.TrimSpace(sub.HDP))
	if hErr != nil || len(hdpRaw) != 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid 32-byte hdp hex"})
		return
	}
	sub.HDP = hex.EncodeToString(hdpRaw)

	// Require fresh signed submission from SellerPub to prevent unauthenticated ProofBundle theft.
	now := time.Now().Unix()
	if sub.TS < now-bboxSkewSeconds || sub.TS > now+bboxSkewSeconds || sub.Sig == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "fresh signed bounty submission (ts + sig) required"})
		return
	}
	if err := bstack.VerifyArtifact(sub.SellerPub, sub.Sig, sub.Signable(sub.HDP)); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bounty submission signature"})
		return
	}

	if !n.allowPubRate(sub.SellerPub) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	b, err := n.store.GetBounty(sub.BountyID)
	if err != nil || !b.Active || b.Filled >= b.MaxFills {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bounty not found or already filled"})
		return
	}
	n.mu.RLock()
	bh := n.bountyHandler
	n.mu.RUnlock()
	if bh == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub bounty buyer is not armed"})
		return
	}
	effReward := EffectiveBountyReward(b)
	if effReward == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bounty reward tapered to zero"})
		return
	}
	b.RewardSats = effReward
	// Atomically reserve bounty slot + unique canonical HDP claim BEFORE executing payout!
	if err := n.store.ReserveBountyClaim(b.ID, sub.HDP, sub.SellerPub, effReward); err != nil {
		if errors.Is(err, ErrBountyDuplicate) {
			if claim, cErr := n.store.GetBountyClaim(sub.HDP); cErr == nil && (claim.Status == "pending" || claim.Status == "broadcast") {
				writeJSON(w, http.StatusAccepted, BountyPayoutResult{
					Accepted:   false,
					BountyID:   claim.BountyID,
					RewardSats: claim.RewardSats,
					TxID:       claim.TxID,
					Status:     claim.Status,
					Reason:     "bounty payout already " + claim.Status + " (awaiting confirmation)",
				})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate fragment HDP already claimed"})
			return
		}
		if errors.Is(err, ErrBountyFilled) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "bounty not found or already filled"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	res, err := bh(r.Context(), b, sub)
	if err != nil {
		// Only release reservation if payout definitely did NOT broadcast (never on ErrBroadcastUnknown!).
		if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
			_ = n.store.CancelBountyClaim(b.ID, sub.HDP)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if res != nil && res.Accepted {
		if n.net == "main" || n.net == "testnet" {
			if mErr := n.store.MarkBountyClaimBroadcast(sub.HDP, res.TxID); mErr != nil {
				log.Printf("hub: MarkBountyClaimBroadcast failed hdp=%s txid=%s: %v", sub.HDP, res.TxID, mErr)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":  "bounty payout broadcast succeeded but failed to record broadcast state: " + mErr.Error(),
					"txid":   res.TxID,
					"status": "pending",
				})
				return
			}
			res.Status = "broadcast"
		} else {
			if fErr := n.store.FinalizeBountyClaim(sub.HDP, res.TxID); fErr != nil {
				log.Printf("hub: FinalizeBountyClaim failed hdp=%s txid=%s: %v", sub.HDP, res.TxID, fErr)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":  "bounty payout succeeded but failed to finalize claim state: " + fErr.Error(),
					"txid":   res.TxID,
					"status": "pending",
				})
				return
			}
			res.Status = "paid"
		}
	} else {
		if cErr := n.store.CancelBountyClaim(b.ID, sub.HDP); cErr != nil {
			log.Printf("hub: CancelBountyClaim failed bounty=%s hdp=%s: %v", b.ID, sub.HDP, cErr)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
