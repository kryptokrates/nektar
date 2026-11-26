package wallet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk/primitives"
)

// FuelKeeper maintains go-wallet-toolbox's two-tier StrategyThroughput UTXO hierarchy
// (default -> reserve -> fuel) completely off the critical path using toolboxwallet.FanOutFuel.
type FuelKeeper struct {
	engine *Engine
	wakeCh chan struct{}
	mu     sync.Mutex
}

func newFuelKeeper(e *Engine) *FuelKeeper {
	return &FuelKeeper{
		engine: e,
		wakeCh: make(chan struct{}, 1),
	}
}

// Wake schedules an asynchronous non-blocking fuel replenishment check.
func (f *FuelKeeper) Wake() {
	select {
	case f.wakeCh <- struct{}{}:
	default:
	}
}

func (f *FuelKeeper) run(ctx context.Context) {
	// Warm fuel stock immediately at startup (no 15s cold-start gap).
	if _, err := f.RunOnce(ctx); err != nil && ctx.Err() == nil {
		f.engine.logf("wallet fuelkeeper: startup warm warning: %v", err)
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.wakeCh:
			if _, err := f.RunOnce(ctx); err != nil && ctx.Err() == nil {
				f.engine.logf("wallet fuelkeeper: wake refill warning: %v", err)
			}
		case <-ticker.C:
			if _, err := f.RunOnce(ctx); err != nil && ctx.Err() == nil {
				f.engine.logf("wallet fuelkeeper: periodic refill warning: %v", err)
			}
		}
	}
}

// waitSpendGuardInterval paces internal zero-external-spend fan-out rounds so consecutive
// FanOutFuel transactions never collide with SpendGuard.MinInterval.
func (f *FuelKeeper) waitSpendGuardInterval(ctx context.Context) error {
	if f.engine == nil || f.engine.cfg.SpendGuardRemainingInterval == nil {
		return nil
	}
	rem := f.engine.cfg.SpendGuardRemainingInterval()
	if rem <= 0 {
		return nil
	}
	timer := time.NewTimer(rem + 5*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RunOnce checks whether proven/total fuel count is below low-water mark and replenishes via FanOutFuel.
func (f *FuelKeeper) RunOnce(ctx context.Context) (int, error) {
	if !f.engine.svc.hasBroadcaster() {
		return 0, nil
	}
	bal, err := f.engine.Balance(ctx)
	if err != nil {
		return 0, err
	}
	prof := f.engine.profile
	lowWater := prof.FuelTarget * prof.LowWaterPct / 100
	if lowWater < 1 {
		lowWater = 1
	}
	// On live wallets with zero confirmed balance, never cannibalize unconfirmed change chains
	// past 1/3 of MaxUnprovenDepth so real user/hub transactions keep ample depth headroom.
	if !f.engine.cfg.InMemoryStore && bal.ConfirmedSats == 0 && f.engine.cfg.MaxUnprovenDepth > 0 {
		fuelDepthCap := max(3, f.engine.cfg.MaxUnprovenDepth/3)
		if bal.MaxUnprovenDepth >= fuelDepthCap && bal.FuelCount >= 2 {
			return 0, nil
		}
	}
	// Gate refill on proven (mined) fuel slots when confirmed balance is available, while also
	// enforcing total fuel low-water so live lanes never hit 0 proven slots.
	needProvenRefill := bal.ProvenFuelCount < lowWater && bal.ConfirmedSats >= f.engine.fuelDenom*2 && bal.FuelCount < prof.FuelTarget
	if bal.FuelCount < lowWater || needProvenRefill {
		return f.WarmFuel(ctx, prof.FuelTarget, prof.FuelSats)
	}
	// Idle dust consolidation: merge fragmented small UTXOs only when fuel stock is healthy.
	if prof.DustThresholdSats > 0 && prof.DustConsolidateTrigger > 0 {
		if dustCount := f.engine.countDustUTXOs(ctx, prof.DustThresholdSats); dustCount >= prof.DustConsolidateTrigger {
			if err := f.waitSpendGuardInterval(ctx); err != nil {
				return 0, err
			}
			if _, cErr := f.engine.Consolidate(ctx, ConsolidateArgs{
				BelowSats: prof.DustThresholdSats,
				MaxInputs: 100,
			}); cErr != nil {
				f.engine.logf("wallet fuelkeeper: idle dust consolidation warning: %v", cErr)
			}
		}
	}
	return 0, nil
}

// WarmFuel synchronously mints exact-denomination fuel UTXOs via go-wallet-toolbox's official
// FanOutFuel(default -> reserve -> fuel) pipeline until at least targetCount fuel UTXOs exist.
func (f *FuelKeeper) WarmFuel(ctx context.Context, targetCount int, _ uint64) (int, error) {
	if targetCount <= 0 {
		targetCount = f.engine.profile.FuelTarget
	}
	// go-wallet-toolbox enforces fuelShape.Satoshis == active DenominationSatoshis!
	fuelSats := f.engine.fuelDenom
	if fuelSats == 0 {
		fuelSats = f.engine.profile.FuelSats
	}
	if !f.engine.svc.hasBroadcaster() {
		return 0, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Step 0: Absorb any static FundAddress() deposits into go-wallet-toolbox's native BRC-29 "default" basket!
	if _, err := f.engine.AbsorbFundingBasket(ctx); err != nil {
		f.engine.logf("wallet fuelkeeper: absorb funding warning: %v", err)
	}

	bal, err := f.engine.Balance(ctx)
	if err != nil {
		return 0, err
	}
	if bal.FuelCount >= targetCount {
		return 0, nil
	}
	need := targetCount - bal.FuelCount
	maxOut := int(f.engine.fanoutMax)
	if maxOut <= 0 {
		maxOut = 64
	}

	// Estimate satoshis needed in reserve to mint `need` fuel UTXOs (including leaf fan-out fees).
	leafTxs := (need + maxOut - 1) / maxOut
	estLeafFee := f.engine.estimateFeeSats(ctx, 2, min(need, maxOut)+1)
	neededReserveSats := uint64(need)*fuelSats + uint64(leafTxs)*(estLeafFee+200)

	// go-wallet-toolbox requires reserve-basket chunks >= fanout_outputs_per_tx * denomination.
	// Only mint reserve chunks when default balance meets that threshold; otherwise leaf FanOutFuel
	// automatically falls back to funding directly from the default basket!
	minReserveChunk := f.engine.fanoutMax * fuelSats
	if bal.ReserveSats < neededReserveSats && bal.DefaultSats > minReserveChunk+2000 {
		chunkSats := max(f.engine.profile.ReserveSats, minReserveChunk+estLeafFee+500)
		availForReserve := bal.DefaultSats - (f.engine.estimateFeeSats(ctx, 4, 8) + 1000)
		if chunkSats > availForReserve {
			chunkSats = availForReserve
		}
		if chunkSats >= minReserveChunk && availForReserve >= chunkSats {
			chunks := int(min(uint64(max(1, leafTxs)), availForReserve/chunkSats))
			if chunks > f.engine.profile.ReserveTarget && f.engine.profile.ReserveTarget > 0 {
				chunks = f.engine.profile.ReserveTarget
			}
			if chunks > maxOut {
				chunks = maxOut
			}
			if chunks >= 1 {
				if err := f.waitSpendGuardInterval(ctx); err != nil {
					return 0, err
				}
				_, rErr := f.engine.tb.FanOutFuel(withExternalSpend(ctx, 0), wdk.ShapedChange{
					Basket:   BasketReserve,
					Count:    uint64(chunks),
					Satoshis: primitives.SatoshiValue(chunkSats),
				}, f.engine.cfg.Originator)
				if rErr != nil {
					return 0, fmt.Errorf("wallet fuelkeeper: fan-out reserve: %w", rErr)
				}
				// go-wallet-toolbox FanOutFuel queues delayed broadcast ('sending'): flush SendWaiting
				// immediately so reserve outputs reach 'unproven'/'completed' before leaf fan-out spends them!
				if _, sErr := f.engine.monitor.SendWaiting(ctx); sErr != nil {
					return 0, fmt.Errorf("wallet fuelkeeper: broadcast reserve fan-out: %w", sErr)
				}
			}
		}
	}

	// Step 2: Mint leaf fuel UTXOs from "reserve" (auto-falling back to "default") via FanOutFuel(BasketFuel)!
	minted := 0
	for minted < need {
		batch := min(need-minted, maxOut)
		balNow, bErr := f.engine.Balance(ctx)
		if bErr != nil {
			return minted, bErr
		}
		if minted > 0 && !f.engine.cfg.InMemoryStore && balNow.ConfirmedSats == 0 && f.engine.cfg.MaxUnprovenDepth > 0 {
			// Avoid chaining multiple unconfirmed fan-out rounds off a single unconfirmed change chain.
			break
		}
		avail := balNow.ReserveSats + balNow.DefaultSats
		batchFee := f.engine.estimateFeeSats(ctx, 2, batch+1) + 200
		if avail > batchFee && uint64(batch)*fuelSats+batchFee > avail {
			maxFit := int((avail - batchFee) / fuelSats)
			if maxFit < batch {
				batch = maxFit
			}
			batchFee = f.engine.estimateFeeSats(ctx, 2, max(1, batch)+1) + 200
			for batch > 1 && uint64(batch)*fuelSats+batchFee > avail {
				batch--
				batchFee = f.engine.estimateFeeSats(ctx, 2, batch+1) + 200
			}
		}
		if batch <= 0 || uint64(batch)*fuelSats+batchFee > avail {
			if minted == 0 {
				return 0, fmt.Errorf("%w: available %d sats < minimum fuel slot (%d + %d fee)", ErrInsufficient, avail, fuelSats, batchFee)
			}
			break
		}
		if err := f.waitSpendGuardInterval(ctx); err != nil {
			return minted, err
		}
		_, lErr := f.engine.tb.FanOutFuel(withExternalSpend(ctx, 0), wdk.ShapedChange{
			Basket:   BasketFuel,
			Count:    uint64(batch),
			Satoshis: primitives.SatoshiValue(fuelSats),
		}, f.engine.cfg.Originator)
		if lErr != nil {
			if minted > 0 {
				return minted, lErr
			}
			return 0, fmt.Errorf("wallet fuelkeeper: fan-out fuel: %w", lErr)
		}
		// Flush queued FanOutFuel immediately so minted fuel UTXOs transition out of 'sending'.
		if _, sErr := f.engine.monitor.SendWaiting(ctx); sErr != nil {
			if minted > 0 {
				return minted, sErr
			}
			return 0, fmt.Errorf("wallet fuelkeeper: broadcast fuel fan-out: %w", sErr)
		}
		minted += batch
	}

	return minted, nil
}
