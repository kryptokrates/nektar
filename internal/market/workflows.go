package market

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	restate "github.com/restatedev/sdk-go"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/memory"
)

const internalTokenHeader = "x-nektar-internal-token"

// Engine coordinates Nektar v0.1 market deals & the active Restate Go SDK durable execution plane.
// SQLite (`deals`, `approvals`, `pending_sales`, `spend_reservations`) is the single verdict authority;
// Restate provides durable execution, retries, awakeables, and timers.
type Engine struct {
	mu              sync.RWMutex
	standingInboxMu sync.Mutex
	standingIndexMu sync.Mutex
	mem             *memory.Service
	policy          *Policy
	saleKey         *ec.PrivateKey // persistent node key signing DisclosureSale offers
	broker          *Broker
	matcher         Matcher
	internalToken   string
}

func NewEngine(mem *memory.Service, pol *Policy) *Engine {
	if pol == nil {
		pol = DefaultPolicy()
	}
	var sk *ec.PrivateKey
	if mem != nil {
		if raw, err := mem.LoadOrCreateProtectedKey("node_market_key_v1", func() ([]byte, error) {
			k, err := ec.NewPrivateKey()
			if err != nil {
				return nil, err
			}
			return k.Serialize(), nil
		}); err == nil && len(raw) == 32 {
			sk, _ = ec.PrivateKeyFromBytes(raw)
			clear(raw)
		}
	}
	if sk == nil {
		var err error
		sk, err = ec.NewPrivateKey()
		if err != nil {
			panic("market: generate fallback sale key: " + err.Error())
		}
	}
	if mem != nil && mem.DB() != nil {
		pol.AttachDB(mem.DB())
	}
	return &Engine{
		mem:     mem,
		policy:  pol,
		saleKey: sk,
	}
}

func (e *Engine) Policy() *Policy { return e.policy }

// SetBroker attaches the live market Broker so Restate BuyerDeal/SellerDeal handlers can publish
// WantAds, collect live SSE offers, and deliver direct/NAT-bbox purchases inside restate.Run.
func (e *Engine) SetBroker(b *Broker) {
	e.mu.Lock()
	e.broker = b
	e.mu.Unlock()
}

func (e *Engine) getBroker() *Broker {
	e.mu.RLock()
	b := e.broker
	e.mu.RUnlock()
	return b
}

func (e *Engine) salePubHex() string {
	if e != nil && e.saleKey != nil {
		return hex.EncodeToString(e.saleKey.PubKey().Compressed())
	}
	if e != nil && e.mem != nil && e.mem.Chain() != nil {
		if pub, err := e.mem.Chain().SigningPubKey(context.Background()); err == nil && pub != nil {
			return hex.EncodeToString(pub.Compressed())
		}
		return e.mem.Chain().IdentityPubHex()
	}
	return ""
}

func (e *Engine) signSaleDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if e != nil && e.saleKey != nil {
		sig, err := e.saleKey.Sign(digest)
		if err != nil {
			return nil, err
		}
		return sig.Serialize(), nil
	}
	if e != nil && e.mem != nil && e.mem.Chain() != nil {
		return e.mem.Chain().SignDigest(ctx, digest)
	}
	return nil, errors.New("market: no signing key available")
}

func (e *Engine) sealKeyAtRest(ctx context.Context, keyHex string) (string, error) {
	if keyHex == "" || strings.HasPrefix(keyHex, marketKeyEncPrefix) {
		return keyHex, nil
	}
	if e == nil || e.mem == nil || e.mem.Chain() == nil {
		return "", errors.New("market: chain port unavailable to encrypt key at rest")
	}
	ct, err := e.mem.Chain().EncryptAtRest(ctx, "market-reveal-key", []byte(keyHex))
	if err != nil || len(ct) == 0 {
		if err == nil {
			err = errors.New("empty ciphertext")
		}
		return "", fmt.Errorf("market: encrypt key at rest: %w", err)
	}
	return marketKeyEncPrefix + hex.EncodeToString(ct), nil
}

// SetMatcher attaches an optional Jev System One semantic Matcher for Restate workflows.
func (e *Engine) SetMatcher(m Matcher) {
	e.mu.Lock()
	e.matcher = m
	e.mu.Unlock()
}

// Matcher returns the attached semantic Matcher (or nil if deterministic-only).
func (e *Engine) Matcher() Matcher {
	e.mu.RLock()
	m := e.matcher
	e.mu.RUnlock()
	return m
}

// SetInternalToken configures an optional shared secret checked on NektarRuntime ingress calls.
func (e *Engine) SetInternalToken(tok string) {
	e.mu.Lock()
	e.internalToken = strings.TrimSpace(tok)
	e.mu.Unlock()
}

func (e *Engine) checkInternalAuth(ctx restate.Context) error {
	return e.checkInternalAuthReq(ctx.Request())
}

func (e *Engine) checkInternalAuthReq(req *restate.Request) error {
	e.mu.RLock()
	want := e.internalToken
	e.mu.RUnlock()
	if want == "" {
		return restate.TerminalErrorf("nektar: internal restate auth token not configured (fail-closed)")
	}
	got := ""
	if req != nil && req.Headers != nil {
		for k, v := range req.Headers.Iter() {
			if strings.EqualFold(k, internalTokenHeader) {
				got = strings.TrimSpace(v)
				break
			}
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return restate.TerminalErrorf("nektar: unauthorized internal restate call")
	}
	return nil
}

// NewRestateServer creates the dedicated h2c Restate service endpoint with optional Request Identity v1 JWT verification.
func (e *Engine) NewRestateServer(identityKeys ...string) *restateserver.Restate {
	srv := restateserver.NewRestate()
	var clean []string
	for _, k := range identityKeys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	if len(clean) > 0 {
		srv = srv.WithIdentityV1(clean...)
	}
	return e.BindRestate(srv)
}

// BindRestate registers ALL Nektar durable services onto restatedev/sdk-go/server.Restate:
//   - Workflow "BuyerDeal"       — durable buyer flow (policy -> awakeable approval -> rank -> Custody -> pay -> claim -> verify -> settle)
//   - Workflow "SellerDeal"      — durable seller flow (BRC-369 seal -> pending_sales -> bounded payment & confirmation wait -> settle)
//   - Virtual Object "Custody"   — private serialized global spend reservation (SQLite atomic tx, zero network I/O)
//   - Virtual Object "Tx"        — private monotonic deduplicated tx lifecycle event journal per txid
//   - Virtual Object "AnchorCadence" — private quantized slot-keyed shard anchoring cadence with PauseOnMaxAttempts
//   - Service "NektarRuntime"    — authenticated ingress gateway to private Tx & AnchorCadence objects
func (e *Engine) BindRestate(srv *restateserver.Restate) *restateserver.Restate {
	dealRetry := restate.WithInvocationRetryPolicy(
		restate.WithInitialRetryInterval(1*time.Second),
		restate.WithRetryIntervalFactor(2.0),
		restate.WithMaxRetryInterval(30*time.Second),
		restate.WithMaxRetryAttempts(8),
		restate.PauseOnMaxAttempts(),
	)
	floorRetry := restate.WithInvocationRetryPolicy(
		restate.WithInitialRetryInterval(2*time.Second),
		restate.WithRetryIntervalFactor(2.0),
		restate.WithMaxRetryInterval(60*time.Second),
		restate.WithMaxRetryAttempts(5),
		restate.PauseOnMaxAttempts(),
	)

	buyerWF := restate.NewWorkflow("BuyerDeal",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateBuyerRun)).
		Handler("deliverKey", restate.NewWorkflowSharedHandler(e.restateBuyerDeliverKey)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	sellerWF := restate.NewWorkflow("SellerDeal",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateSellerRun)).
		Handler("deliverPayment", restate.NewWorkflowSharedHandler(e.restateSellerDeliverPayment)).
		Handler("confirmPayment", restate.NewWorkflowSharedHandler(e.restateSellerConfirmPayment)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	custodyObj := restate.NewObject("Custody",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("reserve", restate.NewObjectHandler(e.restateCustodyReserve)).
		Handler("release", restate.NewObjectHandler(e.restateCustodyRelease)).
		Handler("balance", restate.NewObjectSharedHandler(e.restateCustodyBalance))

	txObj := restate.NewObject("Tx",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("emit", restate.NewObjectHandler(e.restateTxEmit)).
		Handler("state", restate.NewObjectSharedHandler(e.restateTxState))

	cadenceObj := restate.NewObject("AnchorCadence",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(3*time.Minute),
		floorRetry,
	).
		Handler("trigger", restate.NewObjectHandler(e.restateAnchorCadenceTrigger)).
		Handler("status", restate.NewObjectSharedHandler(e.restateAnchorCadenceStatus))

	epistemicObj := restate.NewObject("EpistemicShard",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(3*time.Minute),
		floorRetry,
	).
		Handler("transition", restate.NewObjectHandler(e.restateEpistemicTransition)).
		Handler("anchor", restate.NewObjectHandler(e.restateEpistemicAnchor)).
		Handler("state", restate.NewObjectSharedHandler(e.restateEpistemicState))

	sleepObj := restate.NewObject("SleepCycle",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(5*time.Minute),
		floorRetry,
	).
		Handler("run", restate.NewObjectHandler(e.restateSleepCycleRun)).
		Handler("triggerNow", restate.NewObjectHandler(e.restateSleepCycleTriggerNow)).
		Handler("status", restate.NewObjectSharedHandler(e.restateSleepCycleStatus))

	microEarnerObj := restate.NewObject("MicroEarner",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(5*time.Minute),
		floorRetry,
	).
		Handler("run", restate.NewObjectHandler(e.restateMicroEarnerRun)).
		Handler("toggle", restate.NewObjectHandler(e.restateMicroEarnerToggle)).
		Handler("status", restate.NewObjectSharedHandler(e.restateMicroEarnerStatus))

	runtimeSvc := restate.NewService("NektarRuntime",
		restate.WithInactivityTimeout(1*time.Minute),
		dealRetry,
	).
		Handler("emitTx", restate.NewServiceHandler(e.restateRuntimeEmitTx)).
		Handler("txState", restate.NewServiceHandler(e.restateRuntimeTxState)).
		Handler("triggerAnchor", restate.NewServiceHandler(e.restateRuntimeTriggerAnchor)).
		Handler("anchorStatus", restate.NewServiceHandler(e.restateRuntimeAnchorStatus)).
		Handler("epistemicTransition", restate.NewServiceHandler(e.restateRuntimeEpistemicTransition)).
		Handler("epistemicAnchor", restate.NewServiceHandler(e.restateRuntimeEpistemicAnchor)).
		Handler("epistemicState", restate.NewServiceHandler(e.restateRuntimeEpistemicState)).
		Handler("triggerSleep", restate.NewServiceHandler(e.restateRuntimeTriggerSleep)).
		Handler("sleepStatus", restate.NewServiceHandler(e.restateRuntimeSleepStatus)).
		Handler("runMicroEarner", restate.NewServiceHandler(e.restateRuntimeRunMicroEarner)).
		Handler("toggleMicroEarner", restate.NewServiceHandler(e.restateRuntimeToggleMicroEarner)).
		Handler("microEarnerStatus", restate.NewServiceHandler(e.restateRuntimeMicroEarnerStatus))

	standingWF := restate.NewWorkflow("StandingOrder",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateStandingOrderRun)).
		Handler("onOffer", restate.NewWorkflowSharedHandler(e.restateStandingOrderOnOffer)).
		Handler("steer", restate.NewWorkflowSharedHandler(e.restateStandingOrderSteer)).
		Handler("cancel", restate.NewWorkflowSharedHandler(e.restateStandingOrderCancel)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateStandingOrderStatus))

	guardianWF := restate.NewWorkflow("EscrowGuardian",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateEscrowGuardianRun)).
		Handler("markClaimed", restate.NewWorkflowSharedHandler(e.restateEscrowGuardianMarkClaimed)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateEscrowGuardianStatus))

	return srv.
		Bind(buyerWF).
		Bind(sellerWF).
		Bind(custodyObj).
		Bind(txObj).
		Bind(cadenceObj).
		Bind(epistemicObj).
		Bind(sleepObj).
		Bind(microEarnerObj).
		Bind(runtimeSvc).
		Bind(standingWF).
		Bind(guardianWF)
}

func (e *Engine) restateDealStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (DealState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return DealState{}, err
	}
	return restate.Get[DealState](ctx, "state")
}
