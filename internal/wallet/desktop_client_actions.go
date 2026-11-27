package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
)

type desktopCreateActionArgsWire struct {
	Description string                         `json:"description"`
	InputBEEF   sdkwallet.BytesList            `json:"inputBEEF,omitempty"`
	Inputs      []sdkwallet.CreateActionInput  `json:"inputs,omitempty"`
	Outputs     []sdkwallet.CreateActionOutput `json:"outputs,omitempty"`
	LockTime    *uint32                        `json:"lockTime,omitempty"`
	Version     *uint32                        `json:"version,omitempty"`
	Labels      []string                       `json:"labels,omitempty"`
	Options     *sdkwallet.CreateActionOptions `json:"options,omitempty"`
}

type desktopSignableTxWire struct {
	Tx        flexBytes `json:"tx"`
	Reference flexBytes `json:"reference"`
}

type desktopCreateActionResultWire struct {
	Txid                chainhash.Hash             `json:"txid,omitempty"`
	Tx                  flexBytes                  `json:"tx,omitempty"`
	NoSendChange        []transaction.Outpoint     `json:"noSendChange,omitempty"`
	SendWithResults     []sdkwallet.SendWithResult `json:"sendWithResults,omitempty"`
	SignableTransaction *desktopSignableTxWire     `json:"signableTransaction,omitempty"`
}

func (c *DesktopJSONClient) CreateAction(ctx context.Context, args sdkwallet.CreateActionArgs, originator string) (*sdkwallet.CreateActionResult, error) {
	desc := NormalizeActionDescription(args.Description)
	outs := make([]sdkwallet.CreateActionOutput, len(args.Outputs))
	for i, o := range args.Outputs {
		o.OutputDescription = NormalizeActionDescription(o.OutputDescription)
		if o.OutputDescription == ActionPrefix+"Transaction" {
			o.OutputDescription = desc
		}
		outs[i] = o
	}
	ins := make([]sdkwallet.CreateActionInput, len(args.Inputs))
	for i, in := range args.Inputs {
		if strings.TrimSpace(in.InputDescription) != "" {
			in.InputDescription = NormalizeActionDescription(in.InputDescription)
		} else {
			in.InputDescription = desc
		}
		ins[i] = in
	}
	wire := desktopCreateActionArgsWire{
		Description: desc,
		InputBEEF:   sdkwallet.BytesList(args.InputBEEF),
		Inputs:      ins,
		Outputs:     outs,
		LockTime:    args.LockTime,
		Version:     args.Version,
		Labels:      NormalizeActionLabels(args.Labels...),
		Options:     args.Options,
	}
	data, err := c.call(ctx, "createAction", &wire, originator)
	if err != nil {
		return nil, err
	}
	var raw desktopCreateActionResultWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("desktop wallet createAction decode: %w", err)
	}
	res := &sdkwallet.CreateActionResult{
		Txid:            raw.Txid,
		Tx:              []byte(raw.Tx),
		NoSendChange:    raw.NoSendChange,
		SendWithResults: raw.SendWithResults,
	}
	if raw.SignableTransaction != nil {
		res.SignableTransaction = &sdkwallet.SignableTransaction{
			Tx:        []byte(raw.SignableTransaction.Tx),
			Reference: []byte(raw.SignableTransaction.Reference),
		}
	}
	return res, nil
}

type desktopSignActionResultWire struct {
	Txid            chainhash.Hash             `json:"txid,omitempty"`
	Tx              flexBytes                  `json:"tx,omitempty"`
	SendWithResults []sdkwallet.SendWithResult `json:"sendWithResults,omitempty"`
}

func (c *DesktopJSONClient) SignAction(ctx context.Context, args sdkwallet.SignActionArgs, originator string) (*sdkwallet.SignActionResult, error) {
	data, err := c.call(ctx, "signAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var raw desktopSignActionResultWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("desktop wallet signAction decode: %w", err)
	}
	return &sdkwallet.SignActionResult{
		Txid:            raw.Txid,
		Tx:              []byte(raw.Tx),
		SendWithResults: raw.SendWithResults,
	}, nil
}

func (c *DesktopJSONClient) AbortAction(ctx context.Context, args sdkwallet.AbortActionArgs, originator string) (*sdkwallet.AbortActionResult, error) {
	data, err := c.call(ctx, "abortAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.AbortActionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet abortAction decode: %w", err)
	}
	return &res, nil
}

type desktopListActionsArgsWire struct {
	Labels                           []string            `json:"labels"`
	LabelQueryMode                   sdkwallet.QueryMode `json:"labelQueryMode,omitempty"`
	IncludeLabels                    *bool               `json:"includeLabels,omitempty"`
	IncludeInputs                    *bool               `json:"includeInputs,omitempty"`
	IncludeInputSourceLockingScripts *bool               `json:"includeInputSourceLockingScripts,omitempty"`
	IncludeInputUnlockingScripts     *bool               `json:"includeInputUnlockingScripts,omitempty"`
	IncludeOutputs                   *bool               `json:"includeOutputs,omitempty"`
	IncludeOutputLockingScripts      *bool               `json:"includeOutputLockingScripts,omitempty"`
	Limit                            *uint32             `json:"limit,omitempty"`
	Offset                           *uint32             `json:"offset,omitempty"`
	SeekPermission                   *bool               `json:"seekPermission,omitempty"`
}

func (c *DesktopJSONClient) ListActions(ctx context.Context, args sdkwallet.ListActionsArgs, originator string) (*sdkwallet.ListActionsResult, error) {
	labels := args.Labels
	if len(labels) == 0 {
		labels = []string{"nektar"}
	}
	wire := desktopListActionsArgsWire{
		Labels:                           labels,
		LabelQueryMode:                   args.LabelQueryMode,
		IncludeLabels:                    args.IncludeLabels,
		IncludeInputs:                    args.IncludeInputs,
		IncludeInputSourceLockingScripts: args.IncludeInputSourceLockingScripts,
		IncludeInputUnlockingScripts:     args.IncludeInputUnlockingScripts,
		IncludeOutputs:                   args.IncludeOutputs,
		IncludeOutputLockingScripts:      args.IncludeOutputLockingScripts,
		Limit:                            args.Limit,
		Offset:                           args.Offset,
		SeekPermission:                   args.SeekPermission,
	}
	data, err := c.call(ctx, "listActions", &wire, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ListActionsResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet listActions decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) InternalizeAction(ctx context.Context, args sdkwallet.InternalizeActionArgs, originator string) (*sdkwallet.InternalizeActionResult, error) {
	args.Description = NormalizeActionDescription(args.Description)
	args.Labels = NormalizeActionLabels(args.Labels...)
	data, err := c.call(ctx, "internalizeAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.InternalizeActionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet internalizeAction decode: %w", err)
	}
	return &res, nil
}

type desktopListOutputsArgsWire struct {
	Basket                    string                  `json:"basket"`
	Tags                      []string                `json:"tags,omitempty"`
	TagQueryMode              sdkwallet.QueryMode     `json:"tagQueryMode,omitempty"`
	Include                   sdkwallet.OutputInclude `json:"include,omitempty"`
	IncludeCustomInstructions *bool                   `json:"includeCustomInstructions,omitempty"`
	IncludeTags               *bool                   `json:"includeTags,omitempty"`
	IncludeLabels             *bool                   `json:"includeLabels,omitempty"`
	Limit                     *uint32                 `json:"limit,omitempty"`
	Offset                    *uint32                 `json:"offset,omitempty"`
	SeekPermission            *bool                   `json:"seekPermission,omitempty"`
}

func (c *DesktopJSONClient) ListOutputs(ctx context.Context, args sdkwallet.ListOutputsArgs, originator string) (*sdkwallet.ListOutputsResult, error) {
	wire := desktopListOutputsArgsWire{
		Basket:                    args.Basket,
		Tags:                      args.Tags,
		TagQueryMode:              args.TagQueryMode,
		Include:                   args.Include,
		IncludeCustomInstructions: args.IncludeCustomInstructions,
		IncludeTags:               args.IncludeTags,
		IncludeLabels:             args.IncludeLabels,
		Limit:                     args.Limit,
		Offset:                    args.Offset,
		SeekPermission:            args.SeekPermission,
	}
	data, err := c.call(ctx, "listOutputs", &wire, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ListOutputsResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet listOutputs decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) RelinquishOutput(ctx context.Context, args sdkwallet.RelinquishOutputArgs, originator string) (*sdkwallet.RelinquishOutputResult, error) {
	data, err := c.call(ctx, "relinquishOutput", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.RelinquishOutputResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet relinquishOutput decode: %w", err)
	}
	return &res, nil
}
