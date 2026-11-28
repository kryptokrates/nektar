package market

import "github.com/kryptokrates/nektar/internal/spendpolicy"

// PolicyAutonomyConfig aliases spendpolicy.AutonomyConfig for clean domain separation (ARCH-S2).
type PolicyAutonomyConfig = spendpolicy.AutonomyConfig

// Policy aliases spendpolicy.Policy (DB-authoritative operator spend & sell policy).
type Policy = spendpolicy.Policy

func DefaultPolicy() *Policy {
	return spendpolicy.DefaultPolicy()
}
