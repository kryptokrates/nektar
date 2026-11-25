package market

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	restate "github.com/restatedev/sdk-go"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
)

// DealState records the durable lifecycle of one buyer or seller deal.
type DealState struct {
	DealID       string                    `json:"dealId"`
	Role         string                    `json:"role"`   // buyer | seller
	Status       string                    `json:"status"` // open | offered | paid | payment_pending | claimed | settled | failed | expired
	Want         bstack.WantAd             `json:"want"`
	ChosenOffer  *bstack.OfferAd           `json:"chosenOffer,omitempty"`
	ChirpURL     string                    `json:"chirpUrl,omitempty"`
	SaleOffer    *contracts.KeyedSaleOffer `json:"saleOffer,omitempty"`
	PaymentTxID  string                    `json:"paymentTxid,omitempty"`
	ClaimTxID    string                    `json:"claimTxid,omitempty"`
	DeliveredTxt string                    `json:"deliveredText,omitempty"`
	Error        string                    `json:"error,omitempty"`
	UpdatedAt    int64                     `json:"updatedAt"`
}

// BuyerDealInput starts a Restate BuyerDeal workflow keyed by WantID.
type BuyerDealInput struct {
	Want        bstack.WantAd    `json:"want"`
	Offers      []bstack.OfferAd `json:"offers,omitempty"`
	WaitTimeout time.Duration    `json:"waitTimeout,omitempty"`
}

// SellerDealInput starts a Restate SellerDeal workflow keyed by OfferID.
type SellerDealInput struct {
	Want      bstack.WantAd `json:"want"`
	Shard     string        `json:"shard"`
	Seq       int64         `json:"seq"`
	PriceSats uint64        `json:"priceSats"`
	BuyerPub  string        `json:"buyerPub"`
}

// PaymentSignal delivers a buyer's payment txid to a waiting SellerDeal workflow.
type PaymentSignal struct {
	TxID       string `json:"txid"`
	AmountSats uint64 `json:"amountSats"`
	Confirmed  bool   `json:"confirmed"`
}

// ClaimSignal delivers the seller's BRC-369 revealed content key k to a waiting BuyerDeal workflow.
type ClaimSignal struct {
	ClaimTxID     string `json:"claimTxid"`
	ContentKeyHex string `json:"contentKeyHex"`
}

// CustodyReserveInput serializes UTXO reservation through the Restate Custody Virtual Object.
type CustodyReserveInput struct {
	OperationID string `json:"operationId"`
	AmountSats  uint64 `json:"amountSats"`
}

// CustodyReserveResult reports a serialized reservation from Custody.
type CustodyReserveResult struct {
	Approved    bool   `json:"approved"`
	BalanceSats uint64 `json:"balanceSats"`
	Reason      string `json:"reason,omitempty"`
}

// TxEventInput records a durable transaction lifecycle transition on the Restate Tx Virtual Object
// (replacing the legacy TypeScript txEmit / tx.ts Virtual Object).
type TxEventInput struct {
	Kind        string `json:"kind"` // broadcast | mempool | mined | failed
	Shard       string `json:"shard,omitempty"`
	BlockHeight uint32 `json:"blockHeight,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// TxState is the durable state stored per txid inside the Restate Tx Virtual Object.
type TxState struct {
	TxID        string         `json:"txid"`
	Status      string         `json:"status"`
	BlockHeight uint32         `json:"blockHeight,omitempty"`
	Events      []TxEventInput `json:"events"`
	UpdatedAt   int64          `json:"updatedAt"`
}

// AnchorCadenceInput triggers a durable shard anchor check on the AnchorCadence Virtual Object
// (replacing legacy TypeScript hostplane.ts cadences).
type AnchorCadenceInput struct {
	IntervalSeconds int64 `json:"intervalSeconds,omitempty"`
}

// AnchorCadenceState summarizes the latest durable anchor cadence run for a shard.
type AnchorCadenceState struct {
	Shard      string `json:"shard"`
	LastTxID   string `json:"lastTxid,omitempty"`
	LastRunAt  int64  `json:"lastRunAt"`
	AnchorRuns int64  `json:"anchorRuns"`
}

// Engine coordinates Nektar v0.4 market deals & durable execution plane on native restatedev/sdk-go
// (100% replacing the legacy @restatedev/restate-sdk TypeScript runtime).
type Engine struct {
	mu      sync.RWMutex
	mem     *memory.Service
	policy  *Policy
	saleKey *ec.PrivateKey // session key signing DisclosureSale offers
	deals   map[string]*DealState
}

func NewEngine(mem *memory.Service, pol *Policy) *Engine {
	if pol == nil {
		pol = DefaultPolicy()
	}
	sk, _ := ec.NewPrivateKey()
	return &Engine{
		mem:     mem,
		policy:  pol,
		saleKey: sk,
		deals:   make(map[string]*DealState),
	}
}

func (e *Engine) Policy() *Policy { return e.policy }

func (e *Engine) ListDeals() []DealState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]DealState, 0, len(e.deals))
	for _, d := range e.deals {
		out = append(out, *d)
	}
	return out
}

func (e *Engine) recordDeal(d DealState) {
	d.UpdatedAt = time.Now().Unix()
	e.mu.Lock()
	cp := d
	e.deals[d.DealID] = &cp
	e.mu.Unlock()
}

// BindRestate registers ALL 5 Nektar durable services onto restatedev/sdk-go/server.Restate:
//   - Workflow "BuyerDeal"       — durable buyer flow (rank -> await claim promise or timeout -> settle)
//   - Workflow "SellerDeal"      — durable seller flow (BRC-369 seal -> CHIRP -> await payment promise -> settle)
//   - Virtual Object "Custody"   — serialized UTXO / SpendGuard reservation per custody key
//   - Virtual Object "Tx"        — durable tx lifecycle event journal per txid (replaces TS txEmit)
//   - Virtual Object "AnchorCadence" — durable shard anchoring cadence (replaces TS hostplane.ts)
func (e *Engine) BindRestate(srv *restateserver.Restate) *restateserver.Restate {
	buyerWF := restate.NewWorkflow("BuyerDeal").
		Handler("run", restate.NewWorkflowHandler(e.restateBuyerRun)).
		Handler("deliverKey", restate.NewWorkflowSharedHandler(e.restateBuyerDeliverKey)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	sellerWF := restate.NewWorkflow("SellerDeal").
		Handler("run", restate.NewWorkflowHandler(e.restateSellerRun)).
		Handler("deliverPayment", restate.NewWorkflowSharedHandler(e.restateSellerDeliverPayment)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	custodyObj := restate.NewObject("Custody").
		Handler("reserve", restate.NewObjectHandler(e.restateCustodyReserve)).
		Handler("balance", restate.NewObjectSharedHandler(e.restateCustodyBalance))

	txObj := restate.NewObject("Tx").
		Handler("emit", restate.NewObjectHandler(e.restateTxEmit)).
		Handler("state", restate.NewObjectSharedHandler(e.restateTxState))

	cadenceObj := restate.NewObject("AnchorCadence").
		Handler("trigger", restate.NewObjectHandler(e.restateAnchorCadenceTrigger)).
		Handler("status", restate.NewObjectSharedHandler(e.restateAnchorCadenceStatus))

	return srv.Bind(buyerWF).Bind(sellerWF).Bind(custodyObj).Bind(txObj).Bind(cadenceObj)
}

func (e *Engine) restateBuyerRun(ctx restate.WorkflowContext, in BuyerDealInput) (DealState, error) {
	dealID := restate.Key(ctx)
	st := DealState{DealID: dealID, Role: "buyer", Status: "open", Want: in.Want, UpdatedAt: time.Now().Unix()}
	restate.Set(ctx, "state", st)

	ranked := RankOffers(in.Want, in.Offers, e.policy)
	if len(ranked) == 0 {
		st.Status = "failed"
		st.Error = "no eligible offers under policy"
		restate.Set(ctx, "state", st)
		e.recordDeal(st)
		return st, nil
	}
	chosen := ranked[0].Offer
	st.ChosenOffer = &chosen
	st.ChirpURL = chosen.ChirpURL
	st.Status = "offered"
	restate.Set(ctx, "state", st)

	timeout := in.WaitTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	claimFut := restate.Promise[ClaimSignal](ctx, "claim")
	sleepFut := restate.After(ctx, timeout)
	winner, cancelErr := restate.WaitFirst(ctx, claimFut, sleepFut)
	if cancelErr != nil {
		return st, cancelErr
	}
	if winner == sleepFut {
		st.Status = "expired"
		st.Error = "timed out waiting for seller claim reveal"
		restate.Set(ctx, "state", st)
		e.recordDeal(st)
		return st, nil
	}
	sig, err := claimFut.Result()
	if err != nil {
		return st, err
	}
	st.ClaimTxID = sig.ClaimTxID
	st.Status = "settled"
	restate.Set(ctx, "state", st)
	e.recordDeal(st)
	return st, nil
}

func (e *Engine) restateBuyerDeliverKey(ctx restate.WorkflowSharedContext, sig ClaimSignal) (bool, error) {
	if err := restate.Promise[ClaimSignal](ctx, "claim").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) restateSellerRun(ctx restate.WorkflowContext, in SellerDealInput) (DealState, error) {
	dealID := restate.Key(ctx)
	st := DealState{DealID: dealID, Role: "seller", Status: "open", Want: in.Want, UpdatedAt: time.Now().Unix()}
	restate.Set(ctx, "state", st)

	prep, err := restate.Run(ctx, func(rctx restate.RunContext) (struct {
		Offer    contracts.KeyedSaleOffer
		ChirpURL string
	}, error) {
		offer, _, chirpURL, pErr := e.PrepareDirectSale(rctx, in.Shard, in.Seq, in.PriceSats, in.BuyerPub)
		if pErr != nil {
			return struct {
				Offer    contracts.KeyedSaleOffer
				ChirpURL string
			}{}, pErr
		}
		return struct {
			Offer    contracts.KeyedSaleOffer
			ChirpURL string
		}{Offer: *offer, ChirpURL: chirpURL}, nil
	})
	if err != nil {
		st.Status = "failed"
		st.Error = err.Error()
		restate.Set(ctx, "state", st)
		e.recordDeal(st)
		return st, nil
	}
	st.SaleOffer = &prep.Offer
	st.ChirpURL = prep.ChirpURL
	st.Status = "offered"
	restate.Set(ctx, "state", st)

	paySig, pErr := restate.Promise[PaymentSignal](ctx, "payment").Result()
	if pErr != nil {
		return st, pErr
	}
	if !paySig.Confirmed && paySig.AmountSats > e.policy.ZeroConfMaxSats {
		st.Status = "payment_pending"
		st.PaymentTxID = paySig.TxID
		restate.Set(ctx, "state", st)
		e.recordDeal(st)
		return st, nil
	}
	st.PaymentTxID = paySig.TxID
	st.Status = "settled"
	restate.Set(ctx, "state", st)
	e.recordDeal(st)
	return st, nil
}

func (e *Engine) restateSellerDeliverPayment(ctx restate.WorkflowSharedContext, sig PaymentSignal) (bool, error) {
	if err := restate.Promise[PaymentSignal](ctx, "payment").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) restateDealStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (DealState, error) {
	return restate.Get[DealState](ctx, "state")
}

func (e *Engine) restateCustodyReserve(ctx restate.ObjectContext, in CustodyReserveInput) (CustodyReserveResult, error) {
	if err := e.policy.CanSpend("self", in.AmountSats); err != nil {
		return CustodyReserveResult{Approved: false, Reason: err.Error()}, nil
	}
	spent, err := restate.Get[uint64](ctx, "spent_sats")
	if err != nil {
		return CustodyReserveResult{}, err
	}
	bal := e.mem.Chain().BalanceSats()
	if bal < in.AmountSats {
		return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: "insufficient balance"}, nil
	}
	restate.Set(ctx, "spent_sats", spent+in.AmountSats)
	restate.Set(ctx, "last_op", in.OperationID)
	e.policy.RecordSpend(in.AmountSats)
	return CustodyReserveResult{Approved: true, BalanceSats: bal - in.AmountSats}, nil
}

func (e *Engine) restateCustodyBalance(ctx restate.ObjectSharedContext, _ restate.Void) (uint64, error) {
	return e.mem.Chain().BalanceSats(), nil
}

func (e *Engine) restateTxEmit(ctx restate.ObjectContext, ev TxEventInput) (TxState, error) {
	txid := restate.Key(ctx)
	st, err := restate.Get[TxState](ctx, "tx")
	if err != nil {
		return TxState{}, err
	}
	st.TxID = txid
	st.Status = ev.Kind
	if ev.BlockHeight > 0 {
		st.BlockHeight = ev.BlockHeight
	}
	st.Events = append(st.Events, ev)
	st.UpdatedAt = time.Now().Unix()
	restate.Set(ctx, "tx", st)
	return st, nil
}

func (e *Engine) restateTxState(ctx restate.ObjectSharedContext, _ restate.Void) (TxState, error) {
	return restate.Get[TxState](ctx, "tx")
}

func (e *Engine) restateAnchorCadenceTrigger(ctx restate.ObjectContext, in AnchorCadenceInput) (AnchorCadenceState, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[AnchorCadenceState](ctx, "cadence")
	if err != nil {
		return AnchorCadenceState{}, err
	}
	st.Shard = shard
	txid, runErr := restate.Run(ctx, func(rctx restate.RunContext) (string, error) {
		a, aErr := e.mem.AnchorShard(rctx, shard)
		if aErr != nil || a == nil {
			return "", aErr
		}
		return a.TxID, nil
	})
	if runErr != nil {
		return st, runErr
	}
	if txid != "" {
		st.LastTxID = txid
		st.AnchorRuns++
		restate.ObjectSend(ctx, "Tx", txid, "emit").Send(TxEventInput{Kind: "broadcast", Shard: shard})
	}
	st.LastRunAt = time.Now().Unix()
	restate.Set(ctx, "cadence", st)
	if in.IntervalSeconds > 0 {
		restate.ObjectSend(ctx, "AnchorCadence", shard, "trigger").
			Send(in, restate.WithDelay(time.Duration(in.IntervalSeconds)*time.Second))
	}
	return st, nil
}

func (e *Engine) restateAnchorCadenceStatus(ctx restate.ObjectSharedContext, _ restate.Void) (AnchorCadenceState, error) {
	return restate.Get[AnchorCadenceState](ctx, "cadence")
}

// PrepareDirectSale creates a signed BRC-369 keyed DisclosureSale offer for (shard, seq),
// stores the encrypted ciphertext in CHIRP (BRC-167), and returns the public offer + private reveal key k.
func (e *Engine) PrepareDirectSale(ctx context.Context, shard string, seq int64, priceSats uint64, buyerPub string) (*contracts.KeyedSaleOffer, [32]byte, string, error) {
	recalled, err := e.mem.Recall(ctx, memory.RecallInput{Shard: shard})
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	var target *memory.RecalledFragment
	for i := range recalled {
		if recalled[i].Seq == seq {
			cp := recalled[i]
			target = &cp
			break
		}
	}
	if target == nil {
		return nil, [32]byte{}, "", fmt.Errorf("market: fragment %s#%d not found or unauthorized", shard, seq)
	}
	expiresAt := uint64(time.Now().Add(time.Hour).Unix())
	offer, err := contracts.CreateKeyedSaleOffer(
		e.saleKey,
		[]byte(target.Text),
		priceSats,
		expiresAt+3600,
		2000,
		expiresAt,
		buyerPub,
		target.Leaf,
		true,
	)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	ctBytes, _ := hex.DecodeString(offer.CiphertextHex)
	chirpURL, _, err := bstack.StoreChirp(e.mem.Objects(), ctBytes)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	return offer, offer.ContentKey(), chirpURL, nil
}

// ExecuteAtomicKnowledgeTrade runs a complete end-to-end buyer <-> seller BRC-369 paid disclosure
// trade on the chain: seller seals fragment with BRC-369 keyed commitment & stores ciphertext in CHIRP,
// buyer verifies seller's signed DisclosureSale offer & pays seller on-chain, seller reveals content
// scalar k, buyer verifies both hWrapped & BRC-369 commitment, decrypts CHIRP ciphertext, and commits
// the purchased fact into its own anchored memory shard!
func ExecuteAtomicKnowledgeTrade(
	ctx context.Context,
	buyer *Engine,
	seller *Engine,
	sellerShard string,
	sellerSeq int64,
	buyerShard string,
	priceSats uint64,
) (*DealState, error) {
	if err := buyer.policy.CanSpend(seller.mem.Chain().IdentityPubHex(), priceSats); err != nil {
		return nil, err
	}
	buyerPub := buyer.mem.Chain().IdentityPubHex()

	// 1. Seller prepares signed BRC-369 keyed sale offer + stores ciphertext in CHIRP.
	recalledSeller, _ := seller.mem.Recall(ctx, memory.RecallInput{Shard: sellerShard})
	var sellerLeaf string
	for _, rf := range recalledSeller {
		if rf.Seq == sellerSeq {
			sellerLeaf = rf.Leaf
			break
		}
	}
	saleOffer, revealKey, chirpURL, err := seller.PrepareDirectSale(ctx, sellerShard, sellerSeq, priceSats, buyerPub)
	if err != nil {
		return nil, err
	}

	// 2. Buyer cryptographically verifies the seller's signed DisclosureSale offer & covenant artifact hash!
	if err := contracts.VerifySaleOffer(saleOffer, buyerPub, sellerLeaf); err != nil {
		return nil, fmt.Errorf("market: buyer rejected invalid offer: %w", err)
	}

	// 3. Buyer pays seller on-chain (SpendGuard enforced!).
	sellerPayScript, err := seller.mem.Chain().Signer().FundScript()
	if err != nil {
		return nil, err
	}
	payTx, fee, spend, err := buyer.mem.Chain().BuildFundedTx(ctx, []*transaction.TransactionOutput{
		{Satoshis: priceSats, LockingScript: sellerPayScript},
	})
	if err != nil {
		return nil, fmt.Errorf("market: buyer fund payment: %w", err)
	}
	payTxID, err := buyer.mem.Chain().Broadcast(ctx, payTx, fee, spend)
	if err != nil {
		return nil, fmt.Errorf("market: buyer broadcast payment: %w", err)
	}
	if err := buyer.mem.Chain().AdoptChange(payTx); err != nil {
		return nil, err
	}
	buyer.policy.RecordSpend(priceSats)

	// 4. Buyer fetches ciphertext from seller's CHIRP store (verified by BRC-167 closure),
	//    verifies revealed key k against DisclosureSale hWrapped + BRC-369 KeyCommitment, and decrypts plaintext!
	verifiedCT, err := bstack.ResolveChirp(seller.mem.Objects(), chirpURL)
	if err != nil {
		return nil, fmt.Errorf("market: resolve CHIRP ciphertext: %w", err)
	}
	if hex.EncodeToString(verifiedCT) != saleOffer.CiphertextHex {
		return nil, errors.New("market: CHIRP ciphertext mismatch vs sale offer")
	}
	plaintext, err := contracts.ClaimKeyedSale(saleOffer, revealKey[:])
	if err != nil {
		return nil, fmt.Errorf("market: claim BRC-369 sale: %w", err)
	}

	// 5. Buyer commits the purchased, verified knowledge into its own memory shard!
	if _, err := buyer.mem.Remember(ctx, memory.RememberInput{
		Shard:      buyerShard,
		Text:       string(plaintext),
		Source:     "market:" + seller.mem.Chain().IdentityPubHex()[:12],
		AutoAnchor: true,
	}); err != nil {
		return nil, fmt.Errorf("market: store purchased fact: %w", err)
	}

	deal := DealState{
		DealID:       "deal-" + payTxID[:12],
		Role:         "buyer",
		Status:       "settled",
		ChirpURL:     chirpURL,
		SaleOffer:    saleOffer,
		PaymentTxID:  payTxID,
		DeliveredTxt: string(plaintext),
	}
	buyer.recordDeal(deal)
	sellerDeal := deal
	sellerDeal.Role = "seller"
	seller.recordDeal(sellerDeal)
	return &deal, nil
}
