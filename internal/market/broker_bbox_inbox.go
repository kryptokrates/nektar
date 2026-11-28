package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/bbox/boxrec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/hub"
)

func (b *Broker) loadBboxCursor(hubURL string) int64 {
	var v int64
	if ok, _ := b.mem.DB().GetJSON("bbox_cursor:"+hubURL, &v); ok {
		return v
	}
	return 0
}

func (b *Broker) saveBboxCursor(hubURL string, seq int64) {
	if seq <= 0 {
		return
	}
	_ = b.mem.DB().SetJSON("bbox_cursor:"+hubURL, seq)
}

func (b *Broker) bboxLoop(ctx context.Context) {
	cursors := make(map[string]int64)
	var busSub *eventbus.Subscription
	var busCh <-chan eventbus.Event
	if b.mem != nil && b.mem.EventBus() != nil {
		topics := []string{
			"hub:bbox:" + strings.ToLower(b.PubHex()),
			"hub:admit:tm_bbox_*",
			"fabric:tm_bbox_*",
		}
		if sub := b.mem.EventBus().Subscribe(topics, 32); sub != nil {
			busSub = sub
			busCh = sub.Events()
			defer busSub.Close()
		}
	}

	drainOnce := func() bool {
		now := time.Now().Unix()
		b.mu.Lock()
		b.pruneBboxExpectedLocked(now)
		b.mu.Unlock()

		gotAny := false
		for _, h := range b.disc.ActiveHosts() {
			since, loaded := cursors[h]
			if !loaded {
				since = b.loadBboxCursor(h)
				cursors[h] = since
			}
			u := fmt.Sprintf("%s/bbox/inbox?pub=%s&since=%d", strings.TrimRight(h, "/"), b.PubHex(), since)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				continue
			}
			tsNow := time.Now().Unix()
			nonce := randNonceHex()
			aud := hubAudience(h)
			authPayload := bstack.BboxInboxAuth{
				Pub:   b.PubHex(),
				TS:    tsNow,
				Nonce: nonce,
				Aud:   aud,
				Since: since,
			}
			if inSig, sErr := bstack.SignArtifactWithSigner(b.engine.signer, authPayload); sErr == nil {
				req.Header.Set("X-Nektar-Pub", b.PubHex())
				req.Header.Set("X-Nektar-Ts", strconv.FormatInt(tsNow, 10))
				req.Header.Set("X-Nektar-Nonce", nonce)
				req.Header.Set("X-Nektar-Sig", inSig)
			}
			resp, err := b.httpClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			var parsed struct {
				Messages []hub.BboxMessage `json:"messages"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed)
			resp.Body.Close()
			advanced := false
			for _, m := range parsed.Messages {
				gotAny = true
				b.bboxWakes.Add(1)
				// Only advance bbox_cursor when delivery succeeds; retry transient HTTP/workflow failures cleanly!
				if hErr := b.handleBboxMessage(ctx, h, m); hErr != nil {
					break
				}
				if m.Seq > cursors[h] {
					cursors[h] = m.Seq
					advanced = true
				}
			}
			if advanced {
				b.saveBboxCursor(h, cursors[h])
			}
		}
		return gotAny
	}

	// Initial catch-up drain on boot, then strictly event-driven wakes (EventBus + SSE bbox_wake + 30s safety net).
	_ = drainOnce()
	reconcileTicker := time.NewTicker(30 * time.Second)
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.bboxWakeCh:
			if drainOnce() {
				b.WakeBbox()
			}
		case <-busCh:
			if drainOnce() {
				b.WakeBbox()
			}
		case <-reconcileTicker.C:
			_ = drainOnce()
		}
	}
}

func (b *Broker) hasSeenBboxNonce(sender, nonce string) bool {
	if strings.TrimSpace(nonce) == "" {
		return true
	}
	key := sender + ":" + nonce
	b.mu.RLock()
	_, seen := b.seenBboxNonces[key]
	b.mu.RUnlock()
	return seen
}

func (b *Broker) markSeenBboxNonce(sender, nonce string, now int64) {
	if strings.TrimSpace(nonce) == "" {
		return
	}
	key := sender + ":" + nonce
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seenBboxNonces) >= maxSeenBboxNonces {
		cutoff := now - 600
		removed := 0
		for k, ts := range b.seenBboxNonces {
			if ts < cutoff {
				delete(b.seenBboxNonces, k)
				removed++
			}
		}
		if len(b.seenBboxNonces) >= maxSeenBboxNonces {
			// Drop 25% arbitrary entries in a single pass so subsequent inserts run in O(1) without quadratic scans.
			target := maxSeenBboxNonces * 3 / 4
			for k := range b.seenBboxNonces {
				delete(b.seenBboxNonces, k)
				if len(b.seenBboxNonces) <= target {
					break
				}
			}
		}
		_ = removed
	}
	b.seenBboxNonces[key] = now
}

func (b *Broker) handleBboxMessage(ctx context.Context, hubURL string, m hub.BboxMessage) error {
	// Verify recipient, audience, timestamp, nonce uniqueness, and sender signature before decrypting!
	if m.RecipientPub != b.PubHex() {
		return nil
	}
	now := time.Now().Unix()
	if m.TS < now-300 || m.TS > now+300 || strings.TrimSpace(m.Nonce) == "" {
		return nil
	}
	isLive := b.mem != nil && b.mem.Chain() != nil && b.mem.Chain().Network() != "memory"
	if hubURL != "" && (m.Aud != "" || isLive) && !strings.EqualFold(m.Aud, hubAudience(hubURL)) {
		return nil
	}
	if m.Sig == "" || bstack.VerifyArtifact(m.SenderPub, m.Sig, m.Signable()) != nil {
		return nil
	}
	if b.hasSeenBboxNonce(m.SenderPub, m.Nonce) {
		return nil
	}
	aad := bstack.BboxAAD(m.Office, m.SenderPub, m.RecipientPub, m.DealID, m.Kind)
	var decWallet boxrec.Decrypter
	var sealWallet boxrec.Sealer
	if b.engine != nil && b.engine.signer != nil {
		if w, wErr := b.engine.signer.ProtoWallet(); wErr == nil {
			decWallet = w
			sealWallet = w
		}
	}
	plainBody, decErr := bstack.OpenBboxMessageBody(ctx, decWallet, b.engine.signer, m.Office, m.Kind, m.SenderPub, m.RecipientPub, m.TS, aad, m.Body)
	if decErr != nil {
		return nil
	}
	switch m.Kind {
	case "purchase_beef":
		var pReq PurchaseRequest
		if json.Unmarshal(plainBody, &pReq) != nil {
			return nil
		}
		pResp, err := b.HandlePurchase(ctx, pReq)
		if err != nil {
			pResp = &PurchaseResponse{OK: false, OfferID: pReq.OfferID, SellerPub: b.PubHex(), Error: err.Error()}
		}
		if pResp != nil && len(pResp.CiphertextHex) > bstack.InlineDirectMaxHexLen && pResp.ChirpURL != "" && b.PublicURL() != "" {
			cp := *pResp
			cp.CiphertextHex = ""
			pResp = &cp
		}
		replyTS := time.Now().Unix()
		bodyRaw, _ := json.Marshal(pResp)
		replyAAD := bstack.BboxAAD(m.Office, b.PubHex(), m.SenderPub, m.DealID, "reveal_key")
		sealedReply, encErr := bstack.SealBboxMessageBody(ctx, sealWallet, m.Office, "reveal_key", b.PubHex(), m.SenderPub, replyTS, replyAAD, bodyRaw)
		if encErr != nil {
			return encErr
		}
		replyHosts := b.peerBboxHosts(ctx, m.SenderPub, []string{hubURL})
		var lastReplyErr error
		delivered := false
		for _, rh := range replyHosts {
			replyMsg := hub.BboxMessage{
				Office:       m.Office,
				RecipientPub: m.SenderPub,
				SenderPub:    b.PubHex(),
				DealID:       m.DealID,
				Kind:         "reveal_key",
				TS:           replyTS,
				Nonce:        randNonceHex(),
				Aud:          hubAudience(rh),
				Body:         sealedReply,
			}
			if sig, sErr := bstack.SignArtifactWithSigner(b.engine.signer, replyMsg.Signable()); sErr == nil {
				replyMsg.Sig = sig
			}
			env, _ := json.Marshal(replyMsg)
			resp, dErr := b.disc.PostJSONWithHub402(ctx, strings.TrimRight(rh, "/")+"/bbox/send", "bbox:send", env)
			if dErr != nil {
				lastReplyErr = dErr
				continue
			}
			resp.Body.Close()
			if resp.StatusCode < 300 {
				delivered = true
				break
			}
			lastReplyErr = fmt.Errorf("bbox reveal_key send HTTP %d", resp.StatusCode)
		}
		if !delivered {
			if lastReplyErr != nil {
				return lastReplyErr
			}
			return errors.New("bbox reveal_key send failed on all candidate hubs")
		}
		b.markSeenBboxNonce(m.SenderPub, m.Nonce, now)
	case "reveal_key":
		var pResp PurchaseResponse
		if json.Unmarshal(plainBody, &pResp) != nil {
			return nil
		}
		b.mu.RLock()
		ch := b.bboxReplies[pResp.OfferID]
		expSeller := b.bboxExpected[pResp.OfferID]
		wantID := b.bboxWantByOffer[pResp.OfferID]
		rc := b.restate
		b.mu.RUnlock()
		if wantID == "" {
			wantID = m.DealID
		}
		// Durable anti-spoofing: if RAM bboxExpected is missing (after restart or spoofed OfferID),
		// verify m.SenderPub against the persisted SQLite buyer DealRecord.CounterpartyPub (fail CLOSED!).
		if expSeller == "" && wantID != "" && b.mem != nil && b.mem.DB() != nil {
			if deal, dErr := b.mem.DB().GetDeal(wantID); dErr == nil && deal.Role == "buyer" {
				expSeller = deal.CounterpartyPub
			}
		}
		if expSeller == "" || !strings.EqualFold(m.SenderPub, expSeller) {
			return nil // reject spoofed reveal_key from unknown or non-seller pubkey!
		}
		// Wake suspended Restate BuyerDeal workflow synchronously BEFORE advancing bbox_cursor!
		if rc != nil && wantID != "" && pResp.OK {
			dCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, kErr := rc.DeliverBuyerKey(dCtx, wantID, ClaimSignal{
				ClaimTxID:      pResp.ClaimTxID,
				ClaimTxBEEFHex: pResp.ClaimTxBEEFHex,
				ContentKeyHex:  pResp.ContentKeyHex,
				CiphertextHex:  pResp.CiphertextHex,
				ChirpURL:       pResp.ChirpURL,
				ProofBundle:    pResp.ProofBundle,
				DealReceipt:    pResp.DealReceipt,
			})
			cancel()
			if kErr != nil {
				return kErr
			}
			// Clean up RAM correlation entries after successful delivery so maps never leak unboundedly.
			b.mu.Lock()
			delete(b.bboxExpected, pResp.OfferID)
			delete(b.bboxWantByOffer, pResp.OfferID)
			delete(b.bboxExpectedAt, pResp.OfferID)
			b.mu.Unlock()
		}
		if ch != nil {
			select {
			case ch <- pResp:
			default:
			}
		}
		b.markSeenBboxNonce(m.SenderPub, m.Nonce, now)
	}
	return nil
}
