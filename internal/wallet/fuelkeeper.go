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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.wakeCh:
			_, _ = f.RunOnce(ctx)
		case <-ticker.C:
			_, _ = f.RunOnce(ctx)
		}
	}
}

// RunOnce checks whether fuel count is below low-water mark and replenishes via FanOutFuel.
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
	if bal.FuelCount < lowWater {
		return f.WarmFuel(ctx, prof.FuelTarget, prof.FuelSats)
	}
	// Idle dust consolidation: merge fragmented small UTXOs when count reaches trigger.
	if prof.DustThresholdSats > 0 && prof.DustConsolidateTrigger > 0 {
		if dustCount := f.engine.countDustUTXOs(ctx, prof.DustThresholdSats); dustCount >= prof.DustConsolidateTrigger {
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
				_, rErr := f.engine.tb.FanOutFuel(withExternalSpend(ctx, 0), wdk.ShapedChange{
					Basket:   BasketReserve,
					Count:    uint64(chunks),
					Satoshis: primitives.SatoshiValue(chunkSats),
				}, f.engine.cfg.Originator)
				if rErr != nil {
					return 0, fmt.Errorf("wallet fuelkeeper: fan-out reserve: %w", rErr)
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
		avail := balNow.ReserveSats + balNow.DefaultSats
		batchFee := f.engine.estimateFeeSats(ctx, 2, batch+1) + 200
		for batch > 1 && uint64(batch)*fuelSats+batchFee > avail {
			batch /= 2
			batchFee = f.engine.estimateFeeSats(ctx, 2, batch+1) + 200
		}
		if batch == 0 || uint64(batch)*fuelSats+batchFee > avail {
			break
		}
		_, lErr := f.engine.tb.FanOutFuel(withExternalSpend(ctx, 0), wdk.ShapedChange{
			Basket:   BasketFuel,
			Count:    uint64(batch),
			Satoshis: primitives.SatoshiValue(fuelSats),
		}, f.engine.cfg.Originator)
		if lErr != nil {
			if minted > 0 {
				return minted, nil
			}
			return 0, fmt.Errorf("wallet fuelkeeper: fan-out fuel: %w", lErr)
		}
		minted += batch
	}

	return minted, nil
}
