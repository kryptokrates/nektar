package market

import (
	"context"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

func (e *Engine) restateRuntimeTriggerSleep(ctx restate.Context, in SleepCycleInput) (SleepCycleState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return SleepCycleState{}, err
	}
	handler := "run"
	if in.Force {
		handler = "triggerNow"
	}
	return restate.Object[SleepCycleState](ctx, "SleepCycle", SleepCycleObjectKeyDefault, handler).Request(in)
}

func (e *Engine) restateRuntimeSleepStatus(ctx restate.Context, _ restate.Void) (SleepCycleState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return SleepCycleState{}, err
	}
	return restate.Object[SleepCycleState](ctx, "SleepCycle", SleepCycleObjectKeyDefault, "status").Request(restate.Void{})
}

func (e *Engine) restateRuntimeRunMicroEarner(ctx restate.Context, in MicroEarnerInput) (MicroEarnerState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return MicroEarnerState{}, err
	}
	return restate.Object[MicroEarnerState](ctx, "MicroEarner", MicroEarnerObjectKeyDefault, "run").Request(in)
}

func (e *Engine) restateRuntimeToggleMicroEarner(ctx restate.Context, in MicroEarnerToggleInput) (MicroEarnerState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return MicroEarnerState{}, err
	}
	return restate.Object[MicroEarnerState](ctx, "MicroEarner", MicroEarnerObjectKeyDefault, "toggle").Request(in)
}

func (e *Engine) restateRuntimeMicroEarnerStatus(ctx restate.Context, _ restate.Void) (MicroEarnerState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return MicroEarnerState{}, err
	}
	return restate.Object[MicroEarnerState](ctx, "MicroEarner", MicroEarnerObjectKeyDefault, "status").Request(restate.Void{})
}

// TriggerSleepCycle invokes SleepCycle durably through NektarRuntime ingress.
func (c *RestateClient) TriggerSleepCycle(ctx context.Context, in SleepCycleInput) (*SleepCycleState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[SleepCycleInput, SleepCycleState](c.Ingress, "NektarRuntime", "triggerSleep").
		Request(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TriggerSleepCycleAsync enqueues a durable background SleepCycle run through NektarRuntime.
func (c *RestateClient) TriggerSleepCycleAsync(ctx context.Context, in SleepCycleInput) (string, error) {
	var opts []ingress.SendOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.ServiceSend[SleepCycleInput](c.Ingress, "NektarRuntime", "triggerSleep").
		Send(ctx, in, opts...)
	if err != nil {
		return "", err
	}
	return sr.Id(), nil
}

// SleepCycleStatus queries the durable SleepCycle Virtual Object state via NektarRuntime.
func (c *RestateClient) SleepCycleStatus(ctx context.Context) (*SleepCycleState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[restate.Void, SleepCycleState](c.Ingress, "NektarRuntime", "sleepStatus").
		Request(ctx, restate.Void{}, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RunMicroEarner invokes MicroEarner durably through NektarRuntime ingress.
func (c *RestateClient) RunMicroEarner(ctx context.Context, in MicroEarnerInput) (*MicroEarnerState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[MicroEarnerInput, MicroEarnerState](c.Ingress, "NektarRuntime", "runMicroEarner").
		Request(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ToggleMicroEarner toggles the durable MicroEarner Virtual Object via NektarRuntime.
func (c *RestateClient) ToggleMicroEarner(ctx context.Context, in MicroEarnerToggleInput) (*MicroEarnerState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[MicroEarnerToggleInput, MicroEarnerState](c.Ingress, "NektarRuntime", "toggleMicroEarner").
		Request(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
