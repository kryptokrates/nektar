package chain

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lightwebinc/bcommon/headers"
)

// Canonical official BSV Association BSV Broadcaster (Teranode-native, formerly Arcade) public endpoints.
const (
	DefaultBroadcasterMainnet   = "https://arcade-v2-us-1.bsvblockchain.tech"
	DefaultBroadcasterMainnetEU = "https://arcade-eu-1.bsvblockchain.tech"
	DefaultBroadcasterTestnet   = "https://arcade-testnet-us-1.bsvblockchain.tech"
)

// DefaultSettleSpec returns the default BSV Broadcaster settlement spec for network.
// Mainnet defaults to both US + EU official BSV Association BSV Broadcaster regions for
// automatic round-robin + failover (zero GorillaPool dependency).
func DefaultSettleSpec(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return "broadcaster:" + DefaultBroadcasterMainnet + ",broadcaster:" + DefaultBroadcasterMainnetEU
	case "testnet", "test":
		return "broadcaster:" + DefaultBroadcasterTestnet
	default:
		return ""
	}
}

// NormalizeSettleSpec expands single or comma-separated broadcaster:/arcade: shorthands to the
// official BSV Association BSV Broadcaster endpoints (never GorillaPool).
func NormalizeSettleSpec(spec string) string {
	parts := splitSpecs(spec)
	if len(parts) == 0 {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		switch {
		case s == "arcade:main", s == "broadcaster:main", s == "broadcaster:mainnet":
			out = append(out, "broadcaster:"+DefaultBroadcasterMainnet, "broadcaster:"+DefaultBroadcasterMainnetEU)
		case s == "arcade:test", s == "arcade:testnet", s == "broadcaster:test", s == "broadcaster:testnet":
			out = append(out, "broadcaster:"+DefaultBroadcasterTestnet)
		case strings.HasPrefix(s, "arcade:"):
			out = append(out, "broadcaster:"+strings.TrimPrefix(s, "arcade:"))
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

// DefaultSettleDepth returns the default confirmation depth for a network.
func DefaultSettleDepth(network string) int {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return DefaultMainnetSettleDepth
	case "testnet", "test":
		return DefaultTestnetSettleDepth
	case "regtest":
		return DefaultRegtestSettleDepth
	default:
		return DefaultMemorySettleDepth
	}
}

// DefaultHeaderSources returns the default comma-separated PoW header witnesses for network.
// Mainnet pairs 3 distinct-origin public PoW header services (WhatsOnChain + Babbage Chaintracks v2 +
// BSV Association Arcade Chaintracks v2) for automatic 2-of-3 quorum with early slow-straggler
// cancellation; testnet pairs 2 distinct-origin witnesses (WhatsOnChain + Arcade Testnet).
func DefaultHeaderSources(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return "woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2,arcade:" + DefaultBroadcasterMainnet
	case "testnet", "test":
		return "woc:test,arcade:" + DefaultBroadcasterTestnet
	case "regtest":
		return "native:http://127.0.0.1:8080"
	default:
		return ""
	}
}

// NormalizeSpecOrigin extracts the canonical "scheme://host[:port]" origin from a header, chain, or settle spec.
func NormalizeSpecOrigin(spec string) (string, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", errors.New("empty spec")
	}
	// Strip optional nodeapi method qualifier (tx=, proof=, spend=, known=).
	if m, rest, ok := strings.Cut(s, "="); ok && !strings.Contains(m, ":") {
		s = strings.TrimSpace(rest)
	}
	var rawURL string
	switch {
	case strings.HasPrefix(s, "woc:"):
		net := strings.TrimPrefix(s, "woc:")
		if net != "main" && net != "test" {
			return "", fmt.Errorf("invalid woc network %q", s)
		}
		rawURL = "https://api.whatsonchain.com"
	case s == "arcade:main", s == "broadcaster:main", s == "broadcaster:mainnet":
		rawURL = DefaultBroadcasterMainnet
	case s == "arcade:test", s == "arcade:testnet", s == "broadcaster:test", s == "broadcaster:testnet":
		rawURL = DefaultBroadcasterTestnet
	case strings.HasPrefix(s, "broadcaster:"):
		rawURL = strings.TrimPrefix(s, "broadcaster:")
	case strings.HasPrefix(s, "arcade:"):
		rawURL = strings.TrimPrefix(s, "arcade:")
	case strings.HasPrefix(s, "arc:"):
		rawURL = strings.TrimPrefix(s, "arc:")
	case strings.HasPrefix(s, "chaintracks:"):
		rawURL = strings.TrimPrefix(s, "chaintracks:")
	case strings.HasPrefix(s, "bhs:"):
		rawURL = strings.TrimPrefix(s, "bhs:")
	case strings.HasPrefix(s, "asset:"):
		rawURL = strings.TrimPrefix(s, "asset:")
	case strings.HasPrefix(s, "rpc:"):
		rawURL = strings.TrimPrefix(s, "rpc:")
	case strings.HasPrefix(s, "native:"):
		rawURL = strings.TrimPrefix(s, "native:")
	case strings.HasPrefix(s, "anchor:"):
		rawURL = strings.TrimPrefix(s, "anchor:")
	case strings.HasPrefix(s, "tcp:"):
		addr := strings.TrimPrefix(s, "tcp:")
		if addr == "" {
			return "", fmt.Errorf("invalid tcp spec %q", s)
		}
		return "tcp://" + strings.ToLower(addr), nil
	default:
		rawURL = s
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid origin URL in %q", spec)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}

// ParseHeaderClient parses one header source spec, fixes bcommon/headers network & regtest native:
// handling, and sets the network's proof-of-work difficulty floor.
func ParseHeaderClient(network, singleSpec string, httpClient *http.Client) (*headers.Client, string, error) {
	netNorm := normalizeNetworkName(network)
	spec := strings.TrimSpace(singleSpec)
	if spec == "" {
		return nil, "", fmt.Errorf("chain: empty header source spec")
	}

	// Expand shorthand broadcaster:/arcade: main/test and strip explicit native: prefix (regtest only).
	isExplicitNative := false
	switch {
	case spec == "arcade:main", spec == "broadcaster:main", spec == "broadcaster:mainnet":
		spec = "arcade:" + DefaultBroadcasterMainnet
	case spec == "arcade:test", spec == "arcade:testnet", spec == "broadcaster:test", spec == "broadcaster:testnet":
		spec = "arcade:" + DefaultBroadcasterTestnet
	case strings.HasPrefix(spec, "broadcaster:"):
		spec = "arcade:" + strings.TrimPrefix(spec, "broadcaster:")
	case strings.HasPrefix(spec, "native:"):
		isExplicitNative = true
		spec = strings.TrimPrefix(spec, "native:")
	}

	hc, err := headers.NewSource(spec)
	if err != nil {
		return nil, "", fmt.Errorf("chain: header source %q: %w", singleSpec, err)
	}
	if hc.Kind == headers.Native || isExplicitNative {
		if netNorm == "main" || netNorm == "testnet" {
			return nil, "", fmt.Errorf("%w: zero-PoW Native header source %q is forbidden on live network %q", ErrCircularHeaderAnchor, singleSpec, netNorm)
		}
		hc.Kind = headers.Native
		hc.Network = headers.Regtest
		hc.MinDifficulty = 0
	} else {
		// Enforce network consistency for woc: specs and propagate target network onto chaintracks/bhs/arcade.
		if hc.Kind == headers.WhatsOnChain {
			if (netNorm == "main" && hc.Network != headers.Mainnet) || (netNorm == "testnet" && hc.Network != headers.Testnet) {
				return nil, "", fmt.Errorf("chain: header source %q does not match network %q", singleSpec, netNorm)
			}
		}
		switch netNorm {
		case "main":
			hc.Network = headers.Mainnet
			hc.MinDifficulty = headers.MainnetMinDifficulty
		case "testnet":
			hc.Network = headers.Testnet
			hc.MinDifficulty = TestnetMinDifficulty
		default:
			hc.Network = headers.Regtest
			hc.MinDifficulty = 0
		}
	}
	if httpClient != nil {
		hc.HTTP = httpClient
	}
	origin, err := NormalizeSpecOrigin(singleSpec)
	if err != nil {
		return nil, "", err
	}
	return hc, origin, nil
}

// ValidateHeaderIndependence enforces non-circular, multi-witness PoW header independence on live networks.
// It rejects single-source configs, Native zero-PoW sources, duplicate witness origins, and any config
// where a proof or settler origin can self-attest its own BUMPs without a disjoint header witness.
func ValidateHeaderIndependence(network, headerSpec, chainSpec, settleSpec string) error {
	netNorm := normalizeNetworkName(network)
	if netNorm == "memory" {
		return nil
	}
	parts := splitSpecs(headerSpec)
	if len(parts) == 0 {
		return fmt.Errorf("%w: no header sources configured for %s", ErrCircularHeaderAnchor, netNorm)
	}
	if netNorm == "regtest" {
		for _, p := range parts {
			if strings.HasPrefix(p, "anchor:") {
				if _, err := NormalizeSpecOrigin(p); err != nil {
					return fmt.Errorf("chain: header source %q: %w", p, err)
				}
				continue
			}
			if _, _, err := ParseHeaderClient(netNorm, p, nil); err != nil {
				return err
			}
		}
		return nil
	}

	// Live network (main / testnet): require distinct-origin PoW sources, zero Native sources.
	headerOrigins := make(map[string]bool, len(parts))
	for _, p := range parts {
		var origin string
		if strings.HasPrefix(p, "anchor:") {
			var err error
			origin, err = NormalizeSpecOrigin(p)
			if err != nil {
				return fmt.Errorf("chain: header source %q: %w", p, err)
			}
		} else {
			hc, orig, err := ParseHeaderClient(netNorm, p, nil)
			if err != nil {
				return err
			}
			if hc.Kind == headers.Native {
				return fmt.Errorf("%w: Native header source %q forbidden on %s", ErrCircularHeaderAnchor, p, netNorm)
			}
			origin = orig
		}
		if headerOrigins[origin] {
			return fmt.Errorf("%w: duplicate header witness origin %q in %q", ErrCircularHeaderAnchor, origin, headerSpec)
		}
		headerOrigins[origin] = true
	}
	if netNorm == "main" || netNorm == "testnet" {
		if len(headerOrigins) < 2 {
			return fmt.Errorf("%w: live network %q requires >=2 independent distinct-origin PoW header witnesses (got %d from %q)",
				ErrCircularHeaderAnchor, netNorm, len(headerOrigins), headerSpec)
		}

		// Verify every proof/settler origin has at least one strictly disjoint header witness on live networks.
		checkDisjoint := func(label, spec string) error {
			for _, item := range splitSpecs(spec) {
				orig, err := NormalizeSpecOrigin(item)
				if err != nil {
					continue
				}
				disjoint := 0
				for hOrig := range headerOrigins {
					if hOrig != orig {
						disjoint++
					}
				}
				if disjoint == 0 {
					return fmt.Errorf("%w: %s origin %q has zero disjoint header witnesses in %q",
						ErrCircularHeaderAnchor, label, orig, headerSpec)
				}
			}
			return nil
		}
		if err := checkDisjoint("chainSpec", chainSpec); err != nil {
			return err
		}
		if err := checkDisjoint("settleSpec", settleSpec); err != nil {
			return err
		}
	}
	return nil
}

func normalizeNetworkName(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return "main"
	case "test", "testnet":
		return "testnet"
	case "regtest":
		return "regtest"
	default:
		return "memory"
	}
}

func splitSpecs(spec string) []string {
	var out []string
	for _, part := range strings.Split(spec, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
