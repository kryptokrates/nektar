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

// EnsureDeployment checks GET /deployments first and only registers endpointURI when missing
// or when registered services differ (never blindly calling force:true on every restart).
func (c *RestateClient) EnsureDeployment(ctx context.Context, endpointURI string, force bool) (changed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/deployments", nil)
	if err == nil {
		if resp, dErr := c.HTTP.Do(req); dErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var parsed struct {
					Deployments []struct {
						URI      string `json:"uri"`
						Services []struct {
							Name string `json:"name"`
						} `json:"services"`
					} `json:"deployments"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&parsed) == nil {
					target := strings.TrimRight(endpointURI, "/")
					for _, dep := range parsed.Deployments {
						if strings.TrimRight(dep.URI, "/") == target && hasExpectedServices(dep.Services) {
							return false, nil
						}
					}
				}
			}
		}
	}
	if err := c.RegisterDeployment(ctx, endpointURI, force); err != nil {
		return false, err
	}
	return true, nil
}

func hasExpectedServices(svcs []struct {
	Name string `json:"name"`
}) bool {
	got := make(map[string]bool, len(svcs))
	for _, s := range svcs {
		got[s.Name] = true
	}
	for _, want := range []string{"BuyerDeal", "SellerDeal", "Custody", "Tx", "AnchorCadence"} {
		if !got[want] {
			return false
		}
	}
	return true
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

// ReserveForDeal reserves spend under a per-deal Custody key ("deal-<dealId>") so unrelated
// deals never serialize behind a single global Custody lock (restate skill key-discipline).
func (c *RestateClient) ReserveForDeal(ctx context.Context, dealID string, amountSats uint64) (*CustodyReserveResult, error) {
	var out CustodyReserveResult
	key := "deal-" + dealID
	idem := "reserve:" + dealID
	if err := c.Call(ctx, "Custody", key, "reserve", idem, CustodyReserveInput{
		OperationID: dealID,
		AmountSats:  amountSats,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EmitTxEvent sends a durable transaction lifecycle event to the Restate Tx Virtual Object (content-derived idempotency key).
func (c *RestateClient) EmitTxEvent(ctx context.Context, txid string, ev TxEventInput) (*TxState, error) {
	var out TxState
	idem := fmt.Sprintf("tx:%s:%s:%d", txid, ev.Kind, ev.BlockHeight)
	if err := c.Call(ctx, "Tx", txid, "emit", idem, ev, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartBuyerDeal starts a durable BuyerDeal workflow keyed by wantId.
func (c *RestateClient) StartBuyerDeal(ctx context.Context, wantID string, in BuyerDealInput) (*DealState, error) {
	var out DealState
	if err := c.Call(ctx, "BuyerDeal", wantID, "run", "buyer:"+wantID, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverBuyerKey delivers the revealed BRC-369 content scalar with a content-derived idempotency key.
func (c *RestateClient) DeliverBuyerKey(ctx context.Context, wantID string, sig ClaimSignal) (bool, error) {
	var out bool
	idem := "deliver-key:" + wantID + ":" + sig.ClaimTxID
	if err := c.Call(ctx, "BuyerDeal", wantID, "deliverKey", idem, sig, &out); err != nil {
		return false, err
	}
	return out, nil
}

// StartSellerDeal starts a durable SellerDeal workflow keyed by offerId.
func (c *RestateClient) StartSellerDeal(ctx context.Context, offerID string, in SellerDealInput) (*DealState, error) {
	var out DealState
	if err := c.Call(ctx, "SellerDeal", offerID, "run", "seller:"+offerID, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverSellerPayment notifies SellerDeal of buyer payment with a content-derived idempotency key.
func (c *RestateClient) DeliverSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	var out bool
	idem := "deliver-pay:" + offerID + ":" + sig.TxID
	if err := c.Call(ctx, "SellerDeal", offerID, "deliverPayment", idem, sig, &out); err != nil {
		return false, err
	}
	return out, nil
}

// TriggerAnchorCadence triggers the durable AnchorCadence Virtual Object for shard.
func (c *RestateClient) TriggerAnchorCadence(ctx context.Context, shard string, intervalSec int64) (*AnchorCadenceState, error) {
	var out AnchorCadenceState
	if err := c.Call(ctx, "AnchorCadence", shard, "trigger", "", AnchorCadenceInput{IntervalSeconds: intervalSec}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
