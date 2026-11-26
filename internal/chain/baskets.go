package chain

import (
	"context"
	"encoding/json"
	"strings"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type agentIDCtxKey struct{}

// WithAgentID attaches a normalized agent identifier (e.g. "claude", "codex", "hermes")
// to ctx so any downstream BRC-100 transaction automatically carries "agent:<id>" labels
// and "Nektar · [<id>] ..." descriptions in BSV Desktop Activity.
func WithAgentID(ctx context.Context, agentID string) context.Context {
	slug := nwallet.SanitizeAgentID(agentID)
	if slug == "" {
		return ctx
	}
	return context.WithValue(ctx, agentIDCtxKey{}, slug)
}

// AgentIDFromContext extracts the attributed agent slug from ctx, or "" if none.
func AgentIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s, ok := ctx.Value(agentIDCtxKey{}).(string); ok {
		return s
	}
	return ""
}

func decorateActionWithContext(ctx context.Context, desc string, labels []string) (string, []string) {
	meta := nwallet.ActionMeta{
		Description: nwallet.NormalizeActionDescription(desc),
		Labels:      nwallet.NormalizeActionLabels(labels...),
	}
	if agentID := AgentIDFromContext(ctx); agentID != "" {
		meta = nwallet.WithAgentAttribution(meta, agentID)
	}
	return meta.Description, meta.Labels
}

// AssetBasketItem represents a single spendable UTXO tracked inside one of Nektar's
// native BRC-100 asset-class baskets (nektar-anchors, nektar-vaults, nektar-escrows, nektar-carriers).
type AssetBasketItem struct {
	Outpoint           string         `json:"outpoint"`
	TxID               string         `json:"txid"`
	Vout               uint32         `json:"vout"`
	Satoshis           uint64         `json:"satoshis"`
	Spendable          bool           `json:"spendable"`
	Tags               []string       `json:"tags,omitempty"`
	CustomInstructions string         `json:"customInstructions,omitempty"`
	Meta               map[string]any `json:"meta,omitempty"`
}

// AssetBasketSummary summarizes one BRC-100 asset basket from BSV Desktop or embedded wallet.
type AssetBasketSummary struct {
	Basket      string            `json:"basket"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Count       int               `json:"count"`
	TotalSats   uint64            `json:"totalSats"`
	Items       []AssetBasketItem `json:"items"`
	Error       string            `json:"error,omitempty"`
}

var basketDisplayInfo = map[string][2]string{
	nwallet.BasketAnchors:  {"Memory Anchors", "Live Rúnar NektarAnchor shard singletons"},
	nwallet.BasketVaults:   {"Vaults & Bonds", "On-chain NektarAgentVault & CheckpointBond covenants"},
	nwallet.BasketEscrows:  {"Active Escrows", "In-flight DisclosureSale market escrows"},
	nwallet.BasketCarriers: {"Carrier Slots", "PushDrop overlay announcement slots"},
}

// ListAssetBaskets queries Nektar's 4 canonical BRC-100 asset-class baskets directly from
// the active wallet (BSV Desktop in client mode, go-wallet-toolbox in embedded mode).
func (p *Port) ListAssetBaskets(ctx context.Context) []AssetBasketSummary {
	out := make([]AssetBasketSummary, 0, len(nwallet.AllAssetBaskets))
	if p == nil || p.wallet == nil {
		return out
	}
	incCustom := true
	incTags := true
	lim := uint32(100)
	for _, basket := range nwallet.AllAssetBaskets {
		info := basketDisplayInfo[basket]
		sum := AssetBasketSummary{
			Basket:      basket,
			Title:       info[0],
			Description: info[1],
			Items:       []AssetBasketItem{},
		}
		res, err := p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
			Basket:                    basket,
			IncludeCustomInstructions: &incCustom,
			IncludeTags:               &incTags,
			Limit:                     &lim,
		}, Originator)
		if err != nil {
			// Fall back to minimal query if a wallet rejects optional flags.
			res, err = p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
				Basket: basket,
				Limit:  &lim,
			}, Originator)
		}
		if err != nil {
			sum.Error = err.Error()
			out = append(out, sum)
			continue
		}
		for _, o := range res.Outputs {
			if !o.Spendable {
				continue
			}
			item := AssetBasketItem{
				Outpoint:           o.Outpoint.String(),
				TxID:               o.Outpoint.Txid.String(),
				Vout:               o.Outpoint.Index,
				Satoshis:           o.Satoshis,
				Spendable:          o.Spendable,
				Tags:               o.Tags,
				CustomInstructions: o.CustomInstructions,
			}
			if strings.HasPrefix(strings.TrimSpace(o.CustomInstructions), "{") {
				var parsed map[string]any
				if json.Unmarshal([]byte(o.CustomInstructions), &parsed) == nil {
					item.Meta = parsed
				}
			}
			sum.Count++
			sum.TotalSats += o.Satoshis
			sum.Items = append(sum.Items, item)
		}
		out = append(out, sum)
	}
	return out
}

func applyOutputTagSpec(wOuts []sdkwallet.CreateActionOutput, spec *nwallet.OutputTagSpec) {
	if spec == nil || strings.TrimSpace(spec.Basket) == "" || len(wOuts) == 0 {
		return
	}
	for i := range wOuts {
		if i > 0 && !spec.ApplyToAll {
			break
		}
		wOuts[i].Basket = strings.TrimSpace(spec.Basket)
		if len(spec.Tags) > 0 {
			wOuts[i].Tags = nwallet.NormalizeActionLabels(spec.Tags...)
		}
		if spec.CustomInstructions != "" {
			wOuts[i].CustomInstructions = spec.CustomInstructions
		}
	}
}
