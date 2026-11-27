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

	"github.com/kryptokrates/nektar/internal/bstack"
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
	for {
		now := time.Now().Unix()
		b.mu.Lock()
		b.pruneBboxExpectedLocked(now)
		waitingReply := len(b.bboxWantByOffer) > 0
		b.mu.Unlock()
		interval := 3 * time.Second
		if waitingReply {
			interval = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
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
				authPayload := map[string]any{
					"action": "bbox_inbox",
					"pub":    b.PubHex(),
					"ts":     tsNow,
					"nonce":  nonce,
					"aud":    aud,
					"since":  since,
				}
				if inSig, sErr := bstack.SignArtifact(b.engine.saleKey, authPayload); sErr == nil {
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
				for _, m := range parsed.Messages {
					gotAny = true
					b.bboxWakes.Add(1)
					// Only advance bbox_cursor when delivery succeeds; retry transient HTTP/Restate failures cleanly!
					if hErr := b.handleBboxMessage(ctx, h, m); hErr != nil {
						break
					}
					if m.Seq > cursors[h] {
						cursors[h] = m.Seq
						b.saveBboxCursor(h, m.Seq)
					}
				}
			}
			if gotAny {
				// Immediate quick follow-up when a trade message just arrived.
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
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
		for k, ts := range b.seenBboxNonces {
			if ts < cutoff {
				delete(b.seenBboxNonces, k)
			}
		}
		for len(b.seenBboxNonces) >= maxSeenBboxNonces {
			var oldestKey string
			var oldestTS int64
			for k, ts := range b.seenBboxNonces {
				if oldestKey == "" || ts < oldestTS {
					oldestKey = k
					oldestTS = ts
				}
			}
			if oldestKey == "" {
				break
			}
			delete(b.seenBboxNonces, oldestKey)
		}
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
	plainBody, decErr := bstack.OpenBboxBody(b.engine.saleKey, aad, m.Body)
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
		bodyRaw, _ := json.Marshal(pResp)
		replyAAD := bstack.BboxAAD(m.Office, b.PubHex(), m.SenderPub, m.DealID, "reveal_key")
		sealedReply, encErr := bstack.SealBboxBody(m.SenderPub, replyAAD, bodyRaw)
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
				TS:           time.Now().Unix(),
				Nonce:        randNonceHex(),
				Aud:          hubAudience(rh),
				Body:         sealedReply,
			}
			if sig, sErr := bstack.SignArtifact(b.engine.saleKey, replyMsg.Signable()); sErr == nil {
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
