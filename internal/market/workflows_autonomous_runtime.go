package market

import "context"

// TriggerSleepCycleLocal executes a serialized SleepCycle distillation & price-decay pass in-process.
func (e *Engine) TriggerSleepCycleLocal(ctx context.Context, in SleepCycleInput) (*SleepCycleState, error) {
	e.sleepMu.Lock()
	defer e.sleepMu.Unlock()
	return e.RunSleepCycle(ctx, in)
}

// RunMicroEarnerLocal executes a serialized MicroEarner liquidity/bounty pass in-process.
func (e *Engine) RunMicroEarnerLocal(ctx context.Context, in MicroEarnerInput) (*MicroEarnerState, error) {
	e.microMu.Lock()
	defer e.microMu.Unlock()
	st := LoadMicroEarnerState(e.mem)
	if !st.Enabled {
		st.Status = "disabled"
		return &st, nil
	}
	return e.RunMicroEarnerCycle(ctx, in)
}
