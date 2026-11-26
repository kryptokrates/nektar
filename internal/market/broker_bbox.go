package market

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
)

func (b *Broker) signPurchaseRequest(req *PurchaseRequest) {
	if req == nil || b.engine == nil || b.engine.saleKey == nil {
		return
	}
	if req.BuyerPub == "" {
		req.BuyerPub = b.PubHex()
	}
	req.TS = time.Now().Unix()
	if sig, err := bstack.SignArtifact(b.engine.saleKey, req.Signable()); err == nil {
		req.BuyerSig = sig
	}
}

func hubAudience(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(hubURL))
	}
	return strings.ToLower(u.Host)
}

func randNonceHex() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// peerBboxHosts returns the target peer's advertised Hub URLs (from federated `dir` AnnounceAds)
// followed by fallbackHosts, deduplicated, so cross-hub NAT /bbox messages reach the peer's Hub directly!
func (b *Broker) peerBboxHosts(ctx context.Context, peerPub string, fallbackHosts []string) []string {
	peerPub = strings.ToLower(strings.TrimSpace(peerPub))
	seen := make(map[string]bool)
	var out []string
	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	findPeer := func(peers []discovery.PeerInfo) bool {
		for _, p := range peers {
			if strings.EqualFold(p.Pub, peerPub) && len(p.Hosts) > 0 {
				for _, h := range p.Hosts {
					add(h)
				}
				return true
			}
		}
		return false
	}
	if b.disc != nil && peerPub != "" {
		if !findPeer(b.disc.Peers()) {
			dCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			if fresh, err := b.disc.DiscoverPeers(dCtx, nil); err == nil {
				_ = findPeer(fresh)
			}
			cancel()
		}
	}
	for _, h := range fallbackHosts {
		add(h)
	}
	return out
}

// DeliverPurchaseAsync delivers a buyer's AtomicBEEF payment to Seller.
// Direct HTTP returns the seller's immediate PurchaseResponse; NAT bbox sends purchase_beef
// and returns (nil, nil) immediately so Restate BuyerDeal suspends durably on Promise("claim")!
func (b *Broker) DeliverPurchaseAsync(ctx context.Context, chosen bstack.OfferAd, dealID string, pReq PurchaseRequest) (*PurchaseResponse, error) {
	var directErr error
	if chosen.SellerURL != "" {
		pResp, err := b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
		if err == nil && pResp != nil && pResp.OK {
			if pResp.Status == "awaiting_confirmation" || pResp.ContentKeyHex == "" {
				// Register bbox correlation so Seller's post-confirmation reveal_key wakes BuyerDeal!
				b.mu.Lock()
				b.bboxExpected[pReq.OfferID] = chosen.Pub
				b.bboxWantByOffer[pReq.OfferID] = dealID
				b.mu.Unlock()
				return nil, nil
			}
			return pResp, nil
		}
		directErr = err
		if pResp != nil && !pResp.OK && pResp.Error != "" && chosen.BboxOffice == "" {
			return pResp, errors.New(pResp.Error)
		}
	}
	if chosen.BboxOffice != "" {
		hosts := b.peerBboxHosts(ctx, chosen.Pub, b.disc.ActiveHosts())
		b.mu.Lock()
		b.bboxExpected[pReq.OfferID] = chosen.Pub
		b.bboxWantByOffer[pReq.OfferID] = dealID
		b.mu.Unlock()
		bodyRaw, _ := json.Marshal(pReq)
		aad := bstack.BboxAAD(chosen.BboxOffice, b.PubHex(), chosen.Pub, dealID, "purchase_beef")
		sealedBody, encErr := bstack.SealBboxBody(chosen.Pub, aad, bodyRaw)
		if encErr != nil {
			return nil, encErr
		}
		sent := false
		for _, h := range hosts {
			msg := hub.BboxMessage{
				Office:       chosen.BboxOffice,
				RecipientPub: chosen.Pub,
				SenderPub:    b.PubHex(),
				DealID:       dealID,
				Kind:         "purchase_beef",
				TS:           time.Now().Unix(),
				Nonce:        randNonceHex(),
				Aud:          hubAudience(h),
				Body:         sealedBody,
			}
			sig, sErr := bstack.SignArtifact(b.engine.saleKey, msg.Signable())
			if sErr != nil {
				return nil, sErr
			}
			msg.Sig = sig
			env, _ := json.Marshal(msg)
			resp, err := b.disc.PostJSONWithHub402(ctx, strings.TrimRight(h, "/")+"/bbox/send", "bbox:send", env)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					sent = true
					break
				}
			}
		}
		if sent {
			return nil, nil
		}
		if directErr != nil {
			return nil, directErr
		}
		return nil, errors.New("failed to deliver purchase via direct HTTP or hub bbox")
	}
	if directErr != nil {
		return nil, directErr
	}
	return nil, errors.New("seller advertised neither SellerURL nor BboxOffice")
}

func (b *Broker) sendDirectPurchase(ctx context.Context, sellerURL string, req PurchaseRequest) (*PurchaseResponse, error) {
	raw, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sellerURL, "/")+"/v1/market/purchase", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PurchaseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 || !out.OK {
		if out.Error == "" {
			out.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &out, errors.New(out.Error)
	}
	return &out, nil
}

func (b *Broker) sendBboxPurchase(ctx context.Context, hubURL, sellerPub, office, dealID string, pReq PurchaseRequest) (*PurchaseResponse, error) {
	ch := make(chan PurchaseResponse, 1)
	b.mu.Lock()
	b.bboxReplies[pReq.OfferID] = ch
	b.bboxExpected[pReq.OfferID] = sellerPub
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.bboxReplies, pReq.OfferID)
		delete(b.bboxExpected, pReq.OfferID)
		b.mu.Unlock()
	}()

	bodyRaw, _ := json.Marshal(pReq)
	aad := bstack.BboxAAD(office, b.PubHex(), sellerPub, dealID, "purchase_beef")
	sealedBody, encErr := bstack.SealBboxBody(sellerPub, aad, bodyRaw)
	if encErr != nil {
		return nil, encErr
	}
	msg := hub.BboxMessage{
		Office:       office,
		RecipientPub: sellerPub,
		SenderPub:    b.PubHex(),
		DealID:       dealID,
		Kind:         "purchase_beef",
		TS:           time.Now().Unix(),
		Nonce:        randNonceHex(),
		Aud:          hubAudience(hubURL),
		Body:         sealedBody,
	}
	sig, sErr := bstack.SignArtifact(b.engine.saleKey, msg.Signable())
	if sErr != nil {
		return nil, sErr
	}
	msg.Sig = sig
	env, _ := json.Marshal(msg)
	resp, err := b.disc.PostJSONWithHub402(ctx, strings.TrimRight(hubURL, "/")+"/bbox/send", "bbox:send", env)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bbox send HTTP %d", resp.StatusCode)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-ch:
		return &reply, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("bbox: timed out waiting for NAT seller key reveal")
	}
}

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
		b.mu.RLock()
		waitingReply := len(b.bboxReplies) > 0 || len(b.bboxWantByOffer) > 0
		b.mu.RUnlock()
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

func (b *Broker) claimSeenBboxNonce(sender, nonce string, now int64) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
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
	}
	if _, seen := b.seenBboxNonces[key]; seen {
		return false
	}
	b.seenBboxNonces[key] = now
	return true
}

func (b *Broker) handleBboxMessage(ctx context.Context, hubURL string, m hub.BboxMessage) error {
	// Verify recipient, timestamp, nonce uniqueness, and sender signature before decrypting!
	if m.RecipientPub != b.PubHex() {
		return nil
	}
	now := time.Now().Unix()
	if m.TS < now-300 || m.TS > now+300 || strings.TrimSpace(m.Nonce) == "" {
		return nil
	}
	if m.Sig == "" || bstack.VerifyArtifact(m.SenderPub, m.Sig, m.Signable()) != nil {
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
		_ = b.claimSeenBboxNonce(m.SenderPub, m.Nonce, now)
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
			b.mu.Unlock()
		}
		_ = b.claimSeenBboxNonce(m.SenderPub, m.Nonce, now)
		if ch != nil {
			select {
			case ch <- pResp:
			default:
			}
		}
	}
	return nil
}
