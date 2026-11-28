package market

import (
	"context"
	"testing"
	"time"

	"github.com/cschleiden/go-workflows/tester"
	"github.com/stretchr/testify/mock"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
)

func TestVirtualClockSellerDealExpiryAndZeroConfHold(t *testing.T) {
	var acts *workflowActivities

	// 1. Virtual-Clock Drill A: 30-minute SellerDealWorkflow TTL expires deterministically in milliseconds!
	t0 := time.Now()
	wtExp := tester.NewWorkflowTester[DealState](SellerDealWorkflow)
	startVirt := wtExp.Now()

	wtExp.OnActivity(acts.SellerPrepareSale, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(sellerPrepResult{
			Offer:       contracts.KeyedSaleOffer{KeyCommitment: "kc-exp-1"},
			ChirpURL:    "chirp://deadbeef",
			ZeroConfMax: 100,
			NowUnix:     startVirt.Unix(),
		}, nil)
	wtExp.OnActivity(acts.PersistDealState, mock.Anything, mock.Anything).
		Return(startVirt.Add(30*time.Minute).Unix(), nil)

	wtExp.Execute(context.Background(), SellerDealInput{
		Want:     bstack.WantAd{WantID: "want-virt-exp"},
		Shard:    "ops",
		Seq:      1,
		OfferTTL: 30 * time.Minute,
	})
	if !wtExp.WorkflowFinished() {
		t.Fatal("expected SellerDealWorkflow to finish via virtual-clock timer")
	}
	resExp, err := wtExp.WorkflowResult()
	if err != nil {
		t.Fatalf("WorkflowResult error: %v", err)
	}
	if resExp.Status != "expired" {
		t.Fatalf("expected expired status after 30m virtual TTL, got %+v", resExp)
	}
	if wtExp.Now().Sub(startVirt) < 30*time.Minute {
		t.Fatalf("expected simulated clock to advance >=30m, advanced %v", wtExp.Now().Sub(startVirt))
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("expected virtual-clock 30m drill to run in milliseconds, took %v", time.Since(t0))
	}

	// 2. Virtual-Clock Drill B: 0-conf payment above ZeroConfMax enters payment_pending, waits 45 virtual minutes
	// for mined block confirmation signal, and settles deterministically!
	t1 := time.Now()
	wtConf := tester.NewWorkflowTester[DealState](SellerDealWorkflow)
	virtStartB := wtConf.Now()

	wtConf.OnActivity(acts.SellerPrepareSale, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(sellerPrepResult{
			Offer:       contracts.KeyedSaleOffer{KeyCommitment: "kc-conf-1"},
			ChirpURL:    "chirp://feedbeef",
			ZeroConfMax: 100,
			NowUnix:     virtStartB.Unix(),
		}, nil)
	wtConf.OnActivity(acts.PersistDealState, mock.Anything, mock.Anything).
		Return(virtStartB.Add(5*time.Minute).Unix(), nil)
	wtConf.OnActivity(acts.SellerSettle, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(virtStartB.Add(50*time.Minute).Unix(), nil)

	wtConf.ScheduleCallback(5*time.Minute, func() {
		wtConf.SignalWorkflow("payment", PaymentSignal{
			TxID:       "tx-unconfirmed-500",
			AmountSats: 500,
			Confirmed:  false,
		})
	})
	wtConf.ScheduleCallback(50*time.Minute, func() {
		wtConf.SignalWorkflow("payment_confirmed", PaymentSignal{
			TxID:       "tx-unconfirmed-500",
			AmountSats: 500,
			Confirmed:  true,
		})
	})

	wtConf.Execute(context.Background(), SellerDealInput{
		Want:           bstack.WantAd{WantID: "want-virt-conf"},
		Shard:          "ops",
		Seq:            2,
		OfferTTL:       30 * time.Minute,
		ConfirmTimeout: 2 * time.Hour,
	})
	resConf, err := wtConf.WorkflowResult()
	if err != nil {
		t.Fatalf("WorkflowResult error: %v", err)
	}
	if resConf.Status != "settled" || resConf.PaymentTxID != "tx-unconfirmed-500" {
		t.Fatalf("expected settled SellerDeal after virtual confirmation, got %+v", resConf)
	}
	if wtConf.Now().Sub(virtStartB) < 50*time.Minute {
		t.Fatalf("expected virtual clock >=50m, got %v", wtConf.Now().Sub(virtStartB))
	}
	if time.Since(t1) > 2*time.Second {
		t.Fatalf("expected virtual-clock 50m confirmation drill to complete in <2s, took %v", time.Since(t1))
	}
}

func TestVirtualClockEscrowGuardianMultiDayLocktimeRefund(t *testing.T) {
	var acts *workflowActivities

	// Virtual-Clock Drill C (RT-8): Fast-forward 72 simulated hours of hourly EscrowGuardian checks
	// until RefundAfter locktime matures and automatic Refund() completes!
	wallStart := time.Now()
	wt := tester.NewWorkflowTester[EscrowGuardianState](
		EscrowGuardianWorkflow,
		tester.WithMaxHistorySize(50_000),
	)
	virtStart := wt.Now()

	wt.OnActivity(acts.EscrowGuardianSaveState, mock.Anything, mock.Anything).
		Return(true, nil)
	wt.OnActivity(acts.EscrowGuardianCheck, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(escrowCheckResult{
			NowUnix:     virtStart.Unix(),
			Done:        false,
			Status:      "watching",
			NextWaitSec: 3600, // 1 simulated hour per check -> 72 hours total!
		}, nil).
		Times(72)
	wt.OnActivity(acts.EscrowGuardianCheck, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(escrowCheckResult{
			NowUnix:    virtStart.Add(72 * time.Hour).Unix(),
			Done:       true,
			Status:     "refunded",
			RefundTxID: "refund-tx-after-72h",
		}, nil).
		Once()

	wt.Execute(context.Background(), EscrowGuardianInput{
		DealID:      "deal-72h-locktime",
		EscrowTxID:  "escrow-tx-72h",
		RefundAfter: uint64(virtStart.Add(72 * time.Hour).Unix()),
	})
	res, err := wt.WorkflowResult()
	if err != nil {
		t.Fatalf("EscrowGuardianWorkflow virtual-clock error: %v", err)
	}
	if res.Status != "refunded" || res.RefundTxID != "refund-tx-after-72h" {
		t.Fatalf("expected refunded EscrowGuardianState after 72h virtual locktime, got %+v", res)
	}
	if wt.Now().Sub(virtStart) < 72*time.Hour {
		t.Fatalf("expected >=72h simulated clock elapsed, got %v", wt.Now().Sub(virtStart))
	}
	if time.Since(wallStart) > 3*time.Second {
		t.Fatalf("expected 72h virtual-clock drill to run in <3s wall time, took %v", time.Since(wallStart))
	}
}
