package market

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/deals"
	"github.com/kryptokrates/nektar/internal/memory"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// WorkflowRegistrar is satisfied by both *deals.Runtime and go-workflows *registry.Registry (including tester.WorkflowTester.Registry()).
type WorkflowRegistrar interface {
	RegisterWorkflow(wf workflow.Workflow, opts ...registry.RegisterOption) error
	RegisterActivity(a workflow.Activity, opts ...registry.RegisterOption) error
}

// Engine coordinates Nektar v0.1 market deals & the embedded pure-Go go-workflows durable execution plane.
// SQLite (`nektar.sqlite` + `workflows.sqlite` WAL) is the single verdict and workflow journal authority —
// zero external sidecars, zero WSL2 requirement, zero internal HTTP/h2c ports.
type Engine struct {
	mu              sync.RWMutex
	standingInboxMu sync.Mutex
	standingIndexMu sync.Mutex
	txStateMu       sync.Mutex
	cadenceMu       sync.Mutex
	sleepMu         sync.Mutex
	microMu         sync.Mutex

	mem        *memory.Service
	policy     *Policy
	signer     *nwallet.MarketSigner // persistent node signer encapsulated in internal/wallet (ARCH-S1)
	keyInitErr error                 // fail-closed error if protected key decryption failed (RCA-4)
	broker     *Broker
	matcher    Matcher

	wfMu        sync.RWMutex
	wfRuntime   *deals.Runtime
	wfClient    *WorkflowClient
	fastJournal *FastMarketJournal
}

// SetFastJournal attaches a Pebble WAL FastMarketJournal for 1,000+ TPS Buyer/Seller deals.
func (e *Engine) SetFastJournal(j *FastMarketJournal) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.fastJournal = j
	e.mu.Unlock()
}

// FastJournal returns the attached Pebble WAL FastMarketJournal if enabled.
func (e *Engine) FastJournal() *FastMarketJournal {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	j := e.fastJournal
	e.mu.RUnlock()
	return j
}

// NewEngine initializes the market Engine. When mem != nil, protected key loading is fail-closed (RCA-4):
// if decryption fails, it NEVER silently generates a random key that would orphan active escrows/identity.
func NewEngine(mem *memory.Service, pol *Policy) *Engine {
	if pol == nil {
		pol = DefaultPolicy()
	}
	var signer *nwallet.MarketSigner
	var keyErr error
	if mem != nil {
		hasLegacyKey := false
		if mem.DB() != nil {
			hasLegacyKey, _ = mem.DB().HasKV("node_market_key_v1")
		}
		if !hasLegacyKey && mem.Chain() != nil && mem.Chain().MarketSigner() != nil {
			signer = mem.Chain().MarketSigner()
		} else {
			raw, err := mem.LoadOrCreateProtectedKey("node_market_key_v1", nwallet.GenerateProtectedKeyBytes)
			if err != nil {
				keyErr = fmt.Errorf("market: load protected node_market_key_v1 failed (fail-closed): %w", err)
			} else {
				signer, keyErr = nwallet.NewMarketSignerFromBytes(raw)
				if keyErr != nil {
					keyErr = fmt.Errorf("market: invalid protected node_market_key_v1: %w", keyErr)
				}
			}
		}
	} else {
		var err error
		signer, err = nwallet.NewEphemeralMarketSigner()
		if err != nil {
			panic("market: generate ephemeral test sale key: " + err.Error())
		}
	}
	if mem != nil && mem.DB() != nil {
		pol.AttachDB(mem.DB())
	}
	return &Engine{
		mem:        mem,
		policy:     pol,
		signer:     signer,
		keyInitErr: keyErr,
	}
}

// Signer returns the Engine's encapsulated MarketSigner (or nil if uninitialized).
func (e *Engine) Signer() *nwallet.MarketSigner {
	if e == nil {
		return nil
	}
	return e.signer
}

// KeyInitError returns any fail-closed error encountered when unsealing node_market_key_v1.
func (e *Engine) KeyInitError() error {
	if e == nil {
		return errors.New("market: nil engine")
	}
	return e.keyInitErr
}

func (e *Engine) Policy() *Policy { return e.policy }

// SetBroker attaches the live market Broker so BuyerDeal/SellerDeal activities can publish
// WantAds, collect verified offers, and deliver direct/NAT-bbox purchases.
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
	if e != nil && e.signer != nil && e.signer.PubHex() != "" {
		return e.signer.PubHex()
	}
	if e != nil && e.mem != nil && e.mem.Chain() != nil && e.keyInitErr == nil {
		if pub, err := e.mem.Chain().SigningPubKey(context.Background()); err == nil && pub != nil {
			return hex.EncodeToString(pub.Compressed())
		}
		return e.mem.Chain().IdentityPubHex()
	}
	return ""
}

func (e *Engine) signSaleDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if e == nil {
		return nil, errors.New("market: nil engine")
	}
	if e.keyInitErr != nil {
		return nil, e.keyInitErr
	}
	if e.signer != nil {
		return e.signer.SignDigest(digest)
	}
	if e.mem != nil && e.mem.Chain() != nil {
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

// SetMatcher attaches an optional Jev System One semantic Matcher for workflows.
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

// RegisterWorkflowsAndActivities binds BuyerDeal, SellerDeal, StandingOrder, EscrowGuardian, and all
// deterministic activities onto a go-workflows Runtime or virtual-clock WorkflowTester registry.
func (e *Engine) RegisterWorkflowsAndActivities(reg WorkflowRegistrar) error {
	if err := reg.RegisterWorkflow(BuyerDealWorkflow); err != nil {
		return err
	}
	if err := reg.RegisterWorkflow(SellerDealWorkflow); err != nil {
		return err
	}
	if err := reg.RegisterWorkflow(StandingOrderWorkflow); err != nil {
		return err
	}
	if err := reg.RegisterWorkflow(EscrowGuardianWorkflow); err != nil {
		return err
	}
	return reg.RegisterActivity(&workflowActivities{eng: e})
}

// StartWorkflows initializes and starts the embedded pure-Go go-workflows runtime backed by
// <dataDir>/workflows.sqlite (or in-memory SQLite when dataDir == "") BEFORE network listeners accept traffic.
func (e *Engine) StartWorkflows(ctx context.Context, dataDir string) (*WorkflowClient, error) {
	if e == nil {
		return nil, errors.New("market: nil engine")
	}
	if e.keyInitErr != nil {
		return nil, e.keyInitErr
	}
	e.wfMu.Lock()
	if e.wfRuntime != nil && e.wfRuntime.IsRunning() && e.wfClient != nil {
		wc := e.wfClient
		e.wfMu.Unlock()
		return wc, nil
	}
	rt, err := deals.NewRuntime(dataDir)
	if err != nil {
		e.wfMu.Unlock()
		return nil, err
	}
	if err := e.RegisterWorkflowsAndActivities(rt); err != nil {
		_ = rt.Close()
		e.wfMu.Unlock()
		return nil, err
	}
	if err := rt.Start(ctx); err != nil {
		_ = rt.Close()
		e.wfMu.Unlock()
		return nil, err
	}
	wc := NewWorkflowClient(e, rt)
	e.wfRuntime = rt
	e.wfClient = wc
	e.wfMu.Unlock()
	if br := e.getBroker(); br != nil {
		br.SetWorkflowClient(wc)
	}
	return wc, nil
}

// EnsureWorkflows starts an embedded in-memory go-workflows runtime if none was started yet.
func (e *Engine) EnsureWorkflows() (*WorkflowClient, error) {
	return e.StartWorkflows(context.Background(), "")
}

// WorkflowClient returns the attached embedded WorkflowClient (or nil if not started).
func (e *Engine) WorkflowClient() *WorkflowClient {
	if e == nil {
		return nil
	}
	e.wfMu.RLock()
	defer e.wfMu.RUnlock()
	return e.wfClient
}

// WorkflowRuntime returns the underlying *deals.Runtime (or nil if not started).
func (e *Engine) WorkflowRuntime() *deals.Runtime {
	if e == nil {
		return nil
	}
	e.wfMu.RLock()
	defer e.wfMu.RUnlock()
	return e.wfRuntime
}

// CloseWorkflows drains active workflow/activity workers and closes workflows.sqlite cleanly.
func (e *Engine) CloseWorkflows() error {
	if e == nil {
		return nil
	}
	e.wfMu.Lock()
	rt := e.wfRuntime
	e.wfRuntime = nil
	e.wfClient = nil
	e.wfMu.Unlock()
	if rt != nil {
		return rt.Close()
	}
	return nil
}

func dealActivityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{
			MaxAttempts:        6,
			FirstRetryInterval: 500 * time.Millisecond,
			MaxRetryInterval:   15 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
}

func singleAttemptActivityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{
			MaxAttempts: 1,
		},
	}
}

// workflowActivities groups all non-deterministic / I/O activities executed by go-workflows sagas.
type workflowActivities struct {
	eng *Engine
}
