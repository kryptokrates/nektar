package distill

import "github.com/kryptokrates/nektar/internal/policy"

// ScrubVerdict aliases policy.ScrubVerdict so callers across distill, memory, market, and server share one gate.
type ScrubVerdict = policy.ScrubVerdict

const (
	ScrubClean            = policy.ScrubClean
	ScrubRedacted         = policy.ScrubRedacted
	ScrubBlocked          = policy.ScrubBlocked
	ScrubDroppedLowSignal = policy.ScrubDroppedLowSignal
)

// ScrubResult aliases policy.ScrubResult.
type ScrubResult = policy.ScrubResult

// Scrub delegates to policy.Scrub (single authoritative fail-closed secret & PII gate).
func Scrub(text string) ScrubResult {
	return policy.Scrub(text)
}
