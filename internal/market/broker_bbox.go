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
	if _, err := crand.Read(b[:]); err != nil {
		panic("market: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (b *Broker) pruneBboxExpectedLocked(now int64) {
	if len(b.bboxExpectedAt) == 0 {
		return
	}
	cutoff := now - bboxExpectedTTLSeconds
	for offerID, ts := range b.bboxExpectedAt {
		if ts < cutoff {
			delete(b.bboxExpected, offerID)
			delete(b.bboxWantByOffer, offerID)
			delete(b.bboxExpectedAt, offerID)
		}
	}
	for len(b.bboxExpected) >= maxBboxExpected {
		var oldestID string
		var oldestTS int64
		for offerID, ts := range b.bboxExpectedAt {
			if oldestID == "" || ts < oldestTS {
				oldestID = offerID
				oldestTS = ts
			}
		}
		if oldestID == "" {
			for offerID := range b.bboxExpected {
				oldestID = offerID
				break
			}
		}
		if oldestID == "" {
			break
		}
		delete(b.bboxExpected, oldestID)
		delete(b.bboxWantByOffer, oldestID)
		delete(b.bboxExpectedAt, oldestID)
	}
}

func (b *Broker) registerBboxExpectationLocked(offerID, sellerPub, dealID string, now int64) {
	if b.bboxExpectedAt == nil {
		b.bboxExpectedAt = make(map[string]int64)
	}
	b.pruneBboxExpectedLocked(now)
	b.bboxExpected[offerID] = sellerPub
	if dealID != "" {
		b.bboxWantByOffer[offerID] = dealID
	}
	b.bboxExpectedAt[offerID] = now
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
				// Register bbox correlation so Seller's post-confirmation reveal_key wakes BuyerDeal,
				// and return pResp so BuyerDeal extends its claim wait window to match RequiredDepth!
				b.mu.Lock()
				b.registerBboxExpectationLocked(pReq.OfferID, chosen.Pub, dealID, time.Now().Unix())
				b.mu.Unlock()
				return pResp, nil
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
		b.registerBboxExpectationLocked(pReq.OfferID, chosen.Pub, dealID, time.Now().Unix())
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
