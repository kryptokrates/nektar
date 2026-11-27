package market

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// RestateInvocation summarizes a live Restate invocation queried from sys_invocation.
type RestateInvocation struct {
	ID             string `json:"id"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	RetryCount     int64  `json:"retryCount"`
	NextRetryAt    int64  `json:"nextRetryAt,omitempty"`
	ModifiedAt     int64  `json:"modifiedAt,omitempty"`
	LastFailureErr string `json:"lastFailureError,omitempty"`
}

// RestateClient is the pure-Go Restate Ingress (`sdk-go/ingress`) + Admin HTTP client
// driving Nektar's active durable execution plane.
type RestateClient struct {
	IngressURL    string
	AdminURL      string
	InternalToken string
	HTTP          *http.Client
	Ingress       *ingress.Client
	AttachIngress *ingress.Client // Timeout: 0 streaming client bounded by caller ctx for long Attach* calls
}

// NewRestateClient constructs a Restate client backed by official restatedev/sdk-go/ingress
// pointing at ingressURL (e.g. http://127.0.0.1:8111) and adminURL (e.g. http://127.0.0.1:9070).
func NewRestateClient(ingressURL, adminURL string) *RestateClient {
	return NewRestateClientWithToken(ingressURL, adminURL, "")
}

// NewRestateClientWithToken constructs a RestateClient sending an optional internal token on NektarRuntime calls.
func NewRestateClientWithToken(ingressURL, adminURL, internalToken string) *RestateClient {
	if ingressURL == "" {
		ingressURL = "http://127.0.0.1:8080"
	}
	if adminURL == "" {
		adminURL = "http://127.0.0.1:9070"
	}
	ingressURL = strings.TrimRight(ingressURL, "/")
	adminURL = strings.TrimRight(adminURL, "/")
	hc := &http.Client{Timeout: 30 * time.Second}
	attachHC := &http.Client{Timeout: 0}
	ic := ingress.NewClient(ingressURL, ingress.WithHttpClient(hc))
	attachIC := ingress.NewClient(ingressURL, ingress.WithHttpClient(attachHC))
	return &RestateClient{
		IngressURL:    ingressURL,
		AdminURL:      adminURL,
		InternalToken: strings.TrimSpace(internalToken),
		HTTP:          hc,
		Ingress:       ic,
		AttachIngress: attachIC,
	}
}

func (c *RestateClient) runtimeHeaders() map[string]string {
	if c.InternalToken == "" {
		return nil
	}
	return map[string]string{internalTokenHeader: c.InternalToken}
}

// StartBuyerDealAsync submits a BuyerDeal workflow asynchronously via sdk-go/ingress (/send) and returns the invocation ID.
// Restate Workflows are inherently idempotent by workflowID (wantID), so no idempotency-key header is attached.
func (c *RestateClient) StartBuyerDealAsync(ctx context.Context, wantID string, in BuyerDealInput) (string, error) {
	var opts []ingress.SendOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[BuyerDealInput](c.Ingress, "BuyerDeal", wantID, "run").
		Send(ctx, in, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: start BuyerDeal %s: %w", wantID, err)
	}
	return sr.Id(), nil
}

// AttachBuyerDeal attaches to an already-started BuyerDeal workflow via sdk-go/ingress WorkflowHandle.
func (c *RestateClient) AttachBuyerDeal(ctx context.Context, wantID string) (*DealState, error) {
	ic := c.AttachIngress
	if ic == nil {
		ic = c.Ingress
	}
	out, err := ingress.WorkflowHandle[DealState](ic, "BuyerDeal", wantID).Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("restate: attach BuyerDeal %s: %w", wantID, err)
	}
	return &out, nil
}

// BuyerDealStatus queries the shared status handler of a BuyerDeal workflow.
func (c *RestateClient) BuyerDealStatus(ctx context.Context, wantID string) (*DealState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[restate.Void, DealState](c.Ingress, "BuyerDeal", wantID, "status").
		Request(ctx, restate.Void{}, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverBuyerKey delivers the revealed BRC-369 content scalar with a content-derived idempotency key.
func (c *RestateClient) DeliverBuyerKey(ctx context.Context, wantID string, sig ClaimSignal) (bool, error) {
	idem := "deliver-key:" + wantID + ":" + sig.ClaimTxID
	if sig.ClaimTxID == "" && len(sig.ContentKeyHex) >= 12 {
		idem = "deliver-key:" + wantID + ":" + sig.ContentKeyHex[:12]
	}
	opts := []ingress.RequestOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[ClaimSignal, bool](c.Ingress, "BuyerDeal", wantID, "deliverKey").
		Request(ctx, sig, opts...)
	if err != nil {
		return false, err
	}
	return out, nil
}

// StartSellerDealAsync submits a SellerDeal workflow asynchronously via sdk-go/ingress (/send) so the producer never blocks!
// Restate Workflows are inherently idempotent by workflowID (offerID), so no idempotency-key header is attached.
func (c *RestateClient) StartSellerDealAsync(ctx context.Context, offerID string, in SellerDealInput) (string, error) {
	var opts []ingress.SendOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[SellerDealInput](c.Ingress, "SellerDeal", offerID, "run").
		Send(ctx, in, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: start SellerDeal %s: %w", offerID, err)
	}
	return sr.Id(), nil
}

// AttachSellerDeal attaches to a SellerDeal workflow result.
func (c *RestateClient) AttachSellerDeal(ctx context.Context, offerID string) (*DealState, error) {
	ic := c.AttachIngress
	if ic == nil {
		ic = c.Ingress
	}
	out, err := ingress.WorkflowHandle[DealState](ic, "SellerDeal", offerID).Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("restate: attach SellerDeal %s: %w", offerID, err)
	}
	return &out, nil
}

// SellerDealStatus queries the shared status handler of a SellerDeal workflow.
func (c *RestateClient) SellerDealStatus(ctx context.Context, offerID string) (*DealState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[restate.Void, DealState](c.Ingress, "SellerDeal", offerID, "status").
		Request(ctx, restate.Void{}, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverSellerPayment notifies SellerDeal of buyer payment with a confirmation-aware idempotency key (R9).
func (c *RestateClient) DeliverSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	idem := fmt.Sprintf("deliver-pay:%s:%s:%v", offerID, sig.TxID, sig.Confirmed)
	opts := []ingress.RequestOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[PaymentSignal, bool](c.Ingress, "SellerDeal", offerID, "deliverPayment").
		Request(ctx, sig, opts...)
	if err != nil {
		return false, err
	}
	return out, nil
}

// DeliverSellerPaymentAsync notifies SellerDeal of buyer payment via one-way durable WorkflowSend (non-blocking).
func (c *RestateClient) DeliverSellerPaymentAsync(ctx context.Context, offerID string, sig PaymentSignal) (string, error) {
	idem := fmt.Sprintf("deliver-pay:%s:%s:%v", offerID, sig.TxID, sig.Confirmed)
	opts := []ingress.SendOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[PaymentSignal](c.Ingress, "SellerDeal", offerID, "deliverPayment").
		Send(ctx, sig, opts...)
	if err != nil {
		return "", err
	}
	return sr.Id(), nil
}

// ConfirmSellerPayment notifies a SellerDeal waiting in payment_pending that the tx has mined!
func (c *RestateClient) ConfirmSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	sig.Confirmed = true
	idem := fmt.Sprintf("confirm-pay:%s:%s", offerID, sig.TxID)
	opts := []ingress.RequestOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[PaymentSignal, bool](c.Ingress, "SellerDeal", offerID, "confirmPayment").
		Request(ctx, sig, opts...)
	if err != nil {
		return false, err
	}
	return out, nil
}

// ConfirmSellerPaymentAsync notifies a waiting SellerDeal via one-way durable WorkflowSend (non-blocking).
func (c *RestateClient) ConfirmSellerPaymentAsync(ctx context.Context, offerID string, sig PaymentSignal) (string, error) {
	sig.Confirmed = true
	idem := fmt.Sprintf("confirm-pay:%s:%s", offerID, sig.TxID)
	opts := []ingress.SendOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[PaymentSignal](c.Ingress, "SellerDeal", offerID, "confirmPayment").
		Send(ctx, sig, opts...)
	if err != nil {
		return "", err
	}
	return sr.Id(), nil
}

// StartStandingOrderAsync launches a durable StandingOrder workflow (0%-CPU Limit-Order for Knowledge).
func (c *RestateClient) StartStandingOrderAsync(ctx context.Context, orderID string, in StandingOrderInput) (string, error) {
	var opts []ingress.SendOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[StandingOrderInput](c.Ingress, "StandingOrder", orderID, "run").
		Send(ctx, in, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: start StandingOrder %s: %w", orderID, err)
	}
	return sr.Id(), nil
}

// StandingOrderStatus queries the durable state of a StandingOrder workflow.
func (c *RestateClient) StandingOrderStatus(ctx context.Context, orderID string) (*StandingOrderState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[restate.Void, StandingOrderState](c.Ingress, "StandingOrder", orderID, "status").
		Request(ctx, restate.Void{}, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SignalStandingOrderOffer wakes a sleeping StandingOrder with a verified OfferAd via restate.Signal.
func (c *RestateClient) SignalStandingOrderOffer(ctx context.Context, orderID string, off bstack.OfferAd) (bool, error) {
	idem := fmt.Sprintf("so-offer:%s:%s", orderID, off.OfferID)
	opts := []ingress.RequestOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	return ingress.Workflow[bstack.OfferAd, bool](c.Ingress, "StandingOrder", orderID, "onOffer").
		Request(ctx, off, opts...)
}

// SignalStandingOrderOfferAsync dispatches an OfferAd to a StandingOrder via one-way durable WorkflowSend.
func (c *RestateClient) SignalStandingOrderOfferAsync(ctx context.Context, orderID string, off bstack.OfferAd) (string, error) {
	idem := fmt.Sprintf("so-offer:%s:%s", orderID, off.OfferID)
	opts := []ingress.SendOption{restate.WithIdempotencyKey(idem)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[bstack.OfferAd](c.Ingress, "StandingOrder", orderID, "onOffer").
		Send(ctx, off, opts...)
	if err != nil {
		return "", err
	}
	return sr.Id(), nil
}

// SteerStandingOrder live-updates maxSats, pause state, or TTL on a running StandingOrder.
func (c *RestateClient) SteerStandingOrder(ctx context.Context, orderID string, steer SteerStandingOrderInput) (bool, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	return ingress.Workflow[SteerStandingOrderInput, bool](c.Ingress, "StandingOrder", orderID, "steer").
		Request(ctx, steer, opts...)
}

// CancelStandingOrder terminates a watching/paused StandingOrder cleanly.
func (c *RestateClient) CancelStandingOrder(ctx context.Context, orderID, reason string) (bool, error) {
	opts := []ingress.RequestOption{restate.WithIdempotencyKey("so-cancel:" + orderID)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	return ingress.Workflow[string, bool](c.Ingress, "StandingOrder", orderID, "cancel").
		Request(ctx, reason, opts...)
}

// StartEscrowGuardianAsync arms a durable EscrowGuardian workflow keyed by escrowTxID.
func (c *RestateClient) StartEscrowGuardianAsync(ctx context.Context, escrowTxID string, in EscrowGuardianInput) (string, error) {
	var opts []ingress.SendOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.WorkflowSend[EscrowGuardianInput](c.Ingress, "EscrowGuardian", escrowTxID, "run").
		Send(ctx, in, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: start EscrowGuardian %s: %w", escrowTxID, err)
	}
	return sr.Id(), nil
}

// EscrowGuardianStatus queries the durable state of an EscrowGuardian workflow.
func (c *RestateClient) EscrowGuardianStatus(ctx context.Context, escrowTxID string) (*EscrowGuardianState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Workflow[restate.Void, EscrowGuardianState](c.Ingress, "EscrowGuardian", escrowTxID, "status").
		Request(ctx, restate.Void{}, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkEscrowGuardianClaimed disarms an armed EscrowGuardian after successful settlement.
func (c *RestateClient) MarkEscrowGuardianClaimed(ctx context.Context, escrowTxID string) (bool, error) {
	opts := []ingress.RequestOption{restate.WithIdempotencyKey("eg-claimed:" + escrowTxID)}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	return ingress.Workflow[restate.Void, bool](c.Ingress, "EscrowGuardian", escrowTxID, "markClaimed").
		Request(ctx, restate.Void{}, opts...)
}
