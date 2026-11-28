package market

import "github.com/kryptokrates/nektar/internal/spendpolicy"

const (
	AIModeOff      = spendpolicy.AIModeOff
	AIModeAdvisory = spendpolicy.AIModeAdvisory
	AIModeGated    = spendpolicy.AIModeGated
	AIModeStrict   = spendpolicy.AIModeStrict

	PrivacyMetadataOnly       = spendpolicy.PrivacyMetadataOnly
	PrivacyDisclosablePreview = spendpolicy.PrivacyDisclosablePreview

	EscrowModeAuto      = spendpolicy.EscrowModeAuto
	EscrowModeAlways    = spendpolicy.EscrowModeAlways
	EscrowModeOff       = spendpolicy.EscrowModeOff
	DefaultEscrowMinSat = spendpolicy.DefaultEscrowMinSat
)

type AIMatchingPolicy = spendpolicy.AIMatchingPolicy
