package wallet

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ActionPrefix is the uniform prefix applied to every Nektar BRC-100 transaction description
// so users in BSV Desktop's Activity feed can immediately tell which transactions come from
// Nektar and what each one did.
const ActionPrefix = "Nektar · "

const (
	maxActionDescRunes = 128
	maxActionLabelLen  = 64

	// Canonical BRC-100 asset-class baskets inside BSV Desktop / go-wallet-toolbox.
	// These hold Nektar's stateful covenant & carrier UTXOs (never splitting fungible satoshis).
	BasketAnchors  = "nektar-anchors"
	BasketVaults   = "nektar-vaults"
	BasketEscrows  = "nektar-escrows"
	BasketCarriers = "nektar-carriers"
)

// AllAssetBaskets lists Nektar's canonical BRC-100 stateful UTXO baskets in display order.
var AllAssetBaskets = []string{
	BasketAnchors,
	BasketVaults,
	BasketEscrows,
	BasketCarriers,
}

// OutputTagSpec attaches BRC-100 basket, tags, and customInstructions metadata to an output
// (typically output 0 for a Rúnar covenant singleton, or all outputs for a carrier tree).
type OutputTagSpec struct {
	Basket             string
	Tags               []string
	CustomInstructions string
	ApplyToAll         bool // false => output 0 only; true => every output in the tx
}

// ActionMeta bundles the canonical human-readable BRC-100 description and structured labels
// for a single Nektar wallet action. ALL user-facing transaction labels and descriptions
// across the entire Nektar codebase are centralized in this file.
type ActionMeta struct {
	Description string
	Labels      []string
}

// SanitizeAgentID normalizes an agent identifier (e.g. "Claude Code", "codex", "hermes")
// into a compact lowercase slug safe for BRC-100 labels ("agent:<slug>").
func SanitizeAgentID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	// Map common client User-Agent / MCP ClientInfo names to crisp canonical slugs.
	switch {
	case strings.Contains(s, "claude"):
		return "claude"
	case strings.Contains(s, "codex"):
		return "codex"
	case strings.Contains(s, "hermes"):
		return "hermes"
	case strings.Contains(s, "openclaw"):
		return "openclaw"
	case strings.Contains(s, "cursor"):
		return "cursor"
	case strings.Contains(s, "windsurf"):
		return "windsurf"
	case strings.Contains(s, "vscode") || strings.Contains(s, "visual studio"):
		return "vscode"
	case strings.Contains(s, "cline"):
		return "cline"
	case strings.Contains(s, "gemini"):
		return "gemini"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 24 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// WithAgentAttribution decorates an ActionMeta with an agent tag ("Nektar · [claude] ...")
// and adds the "agent:<slug>" BRC-100 label for per-agent Activity filtering.
func WithAgentAttribution(meta ActionMeta, agentID string) ActionMeta {
	slug := SanitizeAgentID(agentID)
	if slug == "" {
		return meta
	}
	desc := NormalizeActionDescription(meta.Description)
	tag := "[" + slug + "] "
	rest := strings.TrimPrefix(desc, ActionPrefix)
	if !strings.HasPrefix(rest, tag) {
		desc = truncateRunes(ActionPrefix+tag+rest, maxActionDescRunes)
	}
	return ActionMeta{
		Description: desc,
		Labels:      NormalizeActionLabels(append(append([]string{}, meta.Labels...), "agent:"+slug)...),
	}
}

// --- Memory Anchoring ---

func ActionAnchorShard(shard string, batchN int64, fragmentCount int) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Anchor shard", fmt.Sprintf("%q (batch #%d, %s)", shard, batchN, PluralizeCount(fragmentCount, "fragment", "fragments"))),
		Labels:      NormalizeActionLabels("anchor"),
	}
}

func ActionAnchorAppend(shard string, batchN int64, fragmentCount int) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Anchor append", fmt.Sprintf("%q (batch #%d, %s)", shard, batchN, PluralizeCount(fragmentCount, "fragment", "fragments"))),
		Labels:      NormalizeActionLabels("anchor", "append"),
	}
}

// --- Knowledge Market & Covenants ---

func ActionBuyKnowledge(offerID string, sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Buy knowledge", fmt.Sprintf("offer %s (%d sats)", offerID, sats)),
		Labels:      NormalizeActionLabels("market", "purchase", "brc29"),
	}
}

func ActionLockEscrow(offerID string, sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Lock disclosure escrow", fmt.Sprintf("offer %s (%d sats)", offerID, sats)),
		Labels:      NormalizeActionLabels("market", "escrow"),
	}
}

func ActionDeployVault(sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Deploy agent vault", fmt.Sprintf("%d sats", sats)),
		Labels:      NormalizeActionLabels("vault"),
	}
}

func ActionOpenBond(sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Open checkpoint bond", fmt.Sprintf("%d sats", sats)),
		Labels:      NormalizeActionLabels("bond"),
	}
}

func ActionCovenantSpend() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Covenant spend", ""),
		Labels:      NormalizeActionLabels("covenant"),
	}
}

// --- Discovery, Directory & Hubs ---

func ActionPublishDirectory() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Publish directory record", ""),
		Labels:      NormalizeActionLabels("directory"),
	}
}

func ActionPublishDirectoryWithFee(totalFeeSats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Publish directory record + hub fee", fmt.Sprintf("%d sats", totalFeeSats)),
		Labels:      NormalizeActionLabels("directory", "hub-fee"),
	}
}

func ActionFundCarrierTree(slots int) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Fund overlay carrier tree", PluralizeCount(slots, "slot", "slots")),
		Labels:      NormalizeActionLabels("directory", "carrier"),
	}
}

func ActionSweepCarrierSlot() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Sweep overlay carrier slot", ""),
		Labels:      NormalizeActionLabels("directory", "carrier", "sweep"),
	}
}

func ActionPayHubFee(action string, sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Pay hub fee", fmt.Sprintf("%s (%d sats)", action, sats)),
		Labels:      NormalizeActionLabels("hub-fee", action),
	}
}

func ActionPayHubBounty(bountyID string) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Pay hub bounty", bountyID),
		Labels:      NormalizeActionLabels("hub", "bounty"),
	}
}

func ActionHubFaucetDrip() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Hub faucet drip", ""),
		Labels:      NormalizeActionLabels("hub", "faucet"),
	}
}

// --- Wallet Bridge & Transfers ---

func ActionFundAgentWallet(sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Fund agent wallet", fmt.Sprintf("%d sats", sats)),
		Labels:      NormalizeActionLabels("fund", "brc29"),
	}
}

func ActionFundAgentWalletOutput(sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Fund agent wallet", fmt.Sprintf("BRC-29 transfer (%d sats)", sats)),
		Labels:      NormalizeActionLabels("fund", "brc29"),
	}
}

func ActionSweepAgentWallet() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Sweep agent wallet", "to BSV Desktop"),
		Labels:      NormalizeActionLabels("sweep", "brc29"),
	}
}

func ActionReceiveSweptFunds() ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Receive swept funds", "from Nektar agent wallet"),
		Labels:      NormalizeActionLabels("sweep", "brc29"),
	}
}

func ActionSendBSV(sats uint64) ActionMeta {
	return ActionMeta{
		Description: FormatActionDescription("Send BSV", fmt.Sprintf("%d sats", sats)),
		Labels:      NormalizeActionLabels("send"),
	}
}

// --- Formatting & Normalization Helpers ---

// FormatActionDescription builds a uniform BRC-100 description:
//   - "Nektar · <action>: <detail>" when detail is non-empty
//   - "Nektar · <action>" when detail is empty
// Truncated safely to stay well within BRC-100's [5, 200] character bounds.
func FormatActionDescription(action, detail string) string {
	act := cleanActionText(action)
	det := cleanActionText(detail)
	if act == "" && det == "" {
		return ActionPrefix + "Transaction"
	}
	var out string
	if act == "" {
		out = ActionPrefix + det
	} else if det == "" {
		out = ActionPrefix + act
	} else {
		out = ActionPrefix + act + ": " + det
	}
	return truncateRunes(out, maxActionDescRunes)
}

// NormalizeActionDescription ensures any non-empty or empty BRC-100 description carries
// the uniform "Nektar · " prefix without duplicating it or exceeding BRC-100 limits.
func NormalizeActionDescription(desc string) string {
	s := cleanActionText(desc)
	if s == "" {
		return ActionPrefix + "Transaction"
	}
	if strings.HasPrefix(s, ActionPrefix) {
		return truncateRunes(s, maxActionDescRunes)
	}
	lower := strings.ToLower(s)
	// Strip legacy lowercase "nektar " / "nektar: " prefixes so we don't emit "Nektar · nektar ...".
	for _, legacy := range []string{"nektar · ", "nektar - ", "nektar: ", "nektar "} {
		if strings.HasPrefix(lower, legacy) {
			rest := strings.TrimSpace(s[len(legacy):])
			if rest != "" {
				s = upperFirst(rest)
				break
			}
		}
	}
	if strings.EqualFold(s, "funded tx") || strings.EqualFold(s, "funded output") {
		s = "Transaction"
	}
	return truncateRunes(ActionPrefix+upperFirst(s), maxActionDescRunes)
}

// NormalizeActionLabels returns deduplicated BRC-100 labels always prefixed with "nektar".
func NormalizeActionLabels(labels ...string) []string {
	seen := map[string]bool{"nektar": true}
	out := []string{"nektar"}
	for _, raw := range labels {
		l := strings.ToLower(cleanActionText(raw))
		if l == "" || seen[l] {
			continue
		}
		if len(l) > maxActionLabelLen {
			l = l[:maxActionLabelLen]
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// PluralizeCount formats a concise count like "1 fragment" / "3 fragments".
func PluralizeCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func cleanActionText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r >= 'a' && r <= 'z' {
		return string(r-('a'-'A')) + s[size:]
	}
	return s
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
