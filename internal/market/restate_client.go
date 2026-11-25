package market

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RestateClient is the pure-Go Restate Ingress + Admin HTTP client replacing the legacy
// TypeScript automation/src/rt/{exec,emit,endpoint}.ts stack.
type RestateClient struct {
	IngressURL string
	AdminURL   string
	HTTP       *http.Client
}

// NewRestateClient constructs a Restate client pointing at ingressURL (e.g. http://127.0.0.1:8080)
// and adminURL (e.g. http://127.0.0.1:9070).
func NewRestateClient(ingressURL, adminURL string) *RestateClient {
	if ingressURL == "" {
		ingressURL = "http://127.0.0.1:8080"
	}
	if adminURL == "" {
		adminURL = "http://127.0.0.1:9070"
	}
	return &RestateClient{
		IngressURL: strings.TrimRight(ingressURL, "/"),
		AdminURL:   strings.TrimRight(adminURL, "/"),
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

// RegisterDeployment registers this Go process's Restate service endpoint URI with the Restate Admin API.
func (c *RestateClient) RegisterDeployment(ctx context.Context, endpointURI string, force bool) error {
	body, _ := json.Marshal(map[string]any{
		"uri":   endpointURI,
		"force": force,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AdminURL+"/deployments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: register deployment %s: %w", endpointURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: register deployment HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Call invokes a Restate service/object/workflow handler synchronously over Restate Ingress with optional idempotency key.
func (c *RestateClient) Call(ctx context.Context, service, key, handler, idempotencyKey string, reqBody, respOut any) error {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	path := "/" + url.PathEscape(service)
	if key != "" {
		path += "/" + url.PathEscape(key)
	}
	path += "/" + url.PathEscape(handler)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IngressURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("idempotency-key", idempotencyKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: call %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: call %s HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if respOut != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(respOut)
	}
	return nil
}

// EmitTxEvent sends a durable transaction lifecycle event to the Restate Tx Virtual Object (Go replacement for TS txEmit).
func (c *RestateClient) EmitTxEvent(ctx context.Context, txid string, ev TxEventInput) (*TxState, error) {
	var out TxState
	if err := c.Call(ctx, "Tx", txid, "emit", "", ev, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartBuyerDeal starts a durable BuyerDeal workflow keyed by wantId.
func (c *RestateClient) StartBuyerDeal(ctx context.Context, wantID string, in BuyerDealInput) (*DealState, error) {
	var out DealState
	if err := c.Call(ctx, "BuyerDeal", wantID, "run", "buyer-"+wantID, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartSellerDeal starts a durable SellerDeal workflow keyed by offerId.
func (c *RestateClient) StartSellerDeal(ctx context.Context, offerID string, in SellerDealInput) (*DealState, error) {
	var out DealState
	if err := c.Call(ctx, "SellerDeal", offerID, "run", "seller-"+offerID, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TriggerAnchorCadence triggers the durable AnchorCadence Virtual Object for shard.
func (c *RestateClient) TriggerAnchorCadence(ctx context.Context, shard string, intervalSec int64) (*AnchorCadenceState, error) {
	var out AnchorCadenceState
	if err := c.Call(ctx, "AnchorCadence", shard, "trigger", "", AnchorCadenceInput{IntervalSeconds: intervalSec}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
