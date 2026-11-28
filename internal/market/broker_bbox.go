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

	"github.com/lightwebinc/bbox/boxrec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
)

type bstackBoxSealer = boxrec.Sealer

func (b *Broker) signPurchaseRequest(req *PurchaseRequest) {
	if req == nil || b.engine == nil || b.engine.signer == nil {
		return
	}
	if req.BuyerPub == "" {
		req.BuyerPub = b.PubHex()
	}
	req.TS = time.Now().Unix()
	if sig, err := bstack.SignArtifactWithSigner(b.engine.signer, req.Signable()); err == nil {
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

type bboxChirpEnvelope struct {
	EnvelopeChirpURL string `json:"envelopeChirpUrl"`
}

func (b *Broker) sealBboxPayloadUnderLimit(
	ctx context.Context,
	office, kind, fromPub, toPub string,
	nowTS int64,
	aad, bodyRaw []byte,
) (json.RawMessage, error) {
	toSeal := bodyRaw
	// Keep plaintext well below Lightweb's 16 KiB `boxrec.MaxContent` ceiling by staging oversized
	// payloads (e.g. multi-ancestor ProofBundles or large PurchaseRequests) onto Hub CHIRP!
	if len(toSeal) > 10<<10 && b.mem != nil && b.mem.Objects() != nil && b.disc != nil {
		if chirpURL, _, hashes, cErr := bstack.StoreChirpWithHashes(b.mem.Objects(), toSeal); cErr == nil && chirpURL != "" {
			b.mem.AllowPublicChirpHashes(hashes...)
			b.disc.PushChirpClosureToHosts(ctx, b.disc.ActiveHosts(), b.mem.Objects(), hashes)
			if wrapped, mErr := json.Marshal(bboxChirpEnvelope{EnvelopeChirpURL: chirpURL}); mErr == nil {
				toSeal = wrapped
			}
		}
	}
	var pw bstackBoxSealer
	if b.engine != nil && b.engine.signer != nil {
		if w, wErr := b.engine.signer.ProtoWallet(); wErr == nil {
			pw = w
		}
	}
	return bstack.SealBboxMessageBody(ctx, pw, office, kind, fromPub, toPub, nowTS, aad, toSeal)
}

func (b *Broker) postOfficeBboxMessage(
	ctx context.Context,
	hosts []string,
	office, recipientPub, dealID, kind string,
	nowTS int64,
	sealedBody json.RawMessage,
) error {
	normOffice := bstack.NormalizeBboxOffice(office)
	nonce := randNonceHex()
	officeAud := "office:" + normOffice
	var lastErr error
	delivered := 0
	for _, h := range hosts {
		msg := hub.BboxMessage{
			Office:       normOffice,
			RecipientPub: recipientPub,
			SenderPub:    b.PubHex(),
			DealID:       dealID,
			Kind:         kind,
			TS:           nowTS,
			Nonce:        nonce,
			Aud:          officeAud,
			Body:         sealedBody,
		}
		sig, sErr := bstack.SignArtifactWithSigner(b.engine.signer, msg.Signable())
		if sErr != nil {
			return sErr
		}
		msg.Sig = sig
		env, _ := json.Marshal(msg)
		resp, err := b.disc.PostJSONWithHub402(ctx, strings.TrimRight(h, "/")+"/bbox/send", "bbox:send", env)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode == http.StatusConflict {
			delivered++
			break
		}
		lastErr = fmt.Errorf("bbox %s send HTTP %d", kind, resp.StatusCode)
	}
	if delivered > 0 {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("bbox %s send failed: no reachable hubs", kind)
}

// DeliverPurchaseAsync delivers a buyer's AtomicBEEF payment to Seller.
// Direct HTTP returns the seller's immediate PurchaseResponse; NAT bbox sends purchase_beef
// (office-bound so Leaf-Spine BRC-149 planes route it without direct cross-Hub HTTP) and returns (nil, nil)
// immediately so BuyerDeal suspends durably on its claim signal!
func (b *Broker) DeliverPurchaseAsync(ctx context.Context, chosen bstack.OfferAd, dealID string, pReq PurchaseRequest) (*PurchaseResponse, error) {
	var directErr error
	if chosen.SellerURL != "" {
		pResp, err := b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
		if err == nil && pResp != nil && pResp.OK {
			if pResp.Status == "awaiting_confirmation" || pResp.ContentKeyHex == "" {
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
		normOffice := bstack.NormalizeBboxOffice(chosen.BboxOffice)
		hosts := b.peerBboxHosts(ctx, "", b.disc.ActiveHosts())
		for _, ph := range b.peerBboxHosts(ctx, chosen.Pub, nil) {
			found := false
			for _, eh := range hosts {
				if eh == ph {
					found = true
					break
				}
			}
			if !found {
				hosts = append(hosts, ph)
			}
		}
		nowTS := time.Now().Unix()
		b.mu.Lock()
		b.registerBboxExpectationLocked(pReq.OfferID, chosen.Pub, dealID, nowTS)
		b.mu.Unlock()
		if pReq.PaymentBEEFHex == "" && len(pReq.PaymentBEEFRaw) > 0 {
			pReq.PaymentBEEFHex = hex.EncodeToString(pReq.PaymentBEEFRaw)
		}
		bodyRaw, _ := json.Marshal(pReq)
		aad := bstack.BboxAAD(normOffice, b.PubHex(), chosen.Pub, dealID, "purchase_beef")
		sealedBody, encErr := b.sealBboxPayloadUnderLimit(ctx, normOffice, "purchase_beef", b.PubHex(), chosen.Pub, nowTS, aad, bodyRaw)
		if encErr != nil {
			return nil, encErr
		}
		if err := b.postOfficeBboxMessage(ctx, hosts, normOffice, chosen.Pub, dealID, "purchase_beef", nowTS, sealedBody); err == nil {
			b.WakeBbox()
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
	target := strings.TrimRight(sellerURL, "/") + "/v1/market/purchase"
	useCBOR := b.engine != nil && b.engine.FastJournal() != nil
	var raw []byte
	var err error
	if useCBOR {
		raw, err = EncodePurchaseRequestCBOR(req)
		if err != nil {
			useCBOR = false
		}
	}
	if !useCBOR {
		if req.PaymentBEEFHex == "" && len(req.PaymentBEEFRaw) > 0 {
			req.PaymentBEEFHex = hex.EncodeToString(req.PaymentBEEFRaw)
		}
		raw, _ = json.Marshal(req)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if useCBOR {
		httpReq.Header.Set("Content-Type", ContentTypeCBOR)
		httpReq.Header.Set("Accept", ContentTypeCBOR)
	} else {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PurchaseResponse
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), ContentTypeCBOR) {
		bodyBytes, rErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if rErr != nil {
			return nil, rErr
		}
		dec, dErr := DecodePurchaseResponseCBOR(bodyBytes)
		if dErr != nil {
			return nil, dErr
		}
		out = *dec
	} else {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
			return nil, err
		}
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

	nowTS := time.Now().Unix()
	bodyRaw, _ := json.Marshal(pReq)
	aad := bstack.BboxAAD(office, b.PubHex(), sellerPub, dealID, "purchase_beef")
	var pw bstackBoxSealer
	if b.engine != nil && b.engine.signer != nil {
		if w, wErr := b.engine.signer.ProtoWallet(); wErr == nil {
			pw = w
		}
	}
	sealedBody, encErr := bstack.SealBboxMessageBody(ctx, pw, office, "purchase_beef", b.PubHex(), sellerPub, nowTS, aad, bodyRaw)
	if encErr != nil {
		return nil, encErr
	}
	msg := hub.BboxMessage{
		Office:       office,
		RecipientPub: sellerPub,
		SenderPub:    b.PubHex(),
		DealID:       dealID,
		Kind:         "purchase_beef",
		TS:           nowTS,
		Nonce:        randNonceHex(),
		Aud:          hubAudience(hubURL),
		Body:         sealedBody,
	}
	sig, sErr := bstack.SignArtifactWithSigner(b.engine.signer, msg.Signable())
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
	b.WakeBbox()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-ch:
		return &reply, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("bbox: timed out waiting for NAT seller key reveal")
	}
}
