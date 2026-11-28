package market

import (
	"context"

	"github.com/kryptokrates/nektar/internal/spendpolicy"
	"github.com/kryptokrates/nektar/internal/store"
)

type PolicyProfile = spendpolicy.PolicyProfile
type PolicyLimitsOverride = spendpolicy.PolicyLimitsOverride

func ComputePolicyHash(roles, domains []string, maxPerDeal, maxPerDay, zeroConfMax uint64, aiPol *AIMatchingPolicy) (string, error) {
	return spendpolicy.ComputePolicyHash(roles, domains, maxPerDeal, maxPerDay, zeroConfMax, aiPol)
}

func ApprovePolicyPreset(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string) (store.OperatorPolicyRecord, error) {
	return spendpolicy.ApprovePolicyPreset(db, pol, preset, domains, approvedBy)
}

func OperatorPolicyDigest(rec store.OperatorPolicyRecord) []byte {
	return spendpolicy.OperatorPolicyDigest(rec)
}

func SignOperatorPolicy(ctx context.Context, operatorPub string, signDigest func(context.Context, []byte) ([]byte, error), rec *store.OperatorPolicyRecord) error {
	return spendpolicy.SignOperatorPolicy(ctx, operatorPub, signDigest, rec)
}

func VerifyOperatorPolicy(rec store.OperatorPolicyRecord, pinnedOperatorPub string) error {
	return spendpolicy.VerifyOperatorPolicy(rec, pinnedOperatorPub)
}

func ApprovePolicyPresetSigned(
	ctx context.Context,
	db *store.DB,
	pol *Policy,
	preset string,
	domains []string,
	approvedBy string,
	aiPol *AIMatchingPolicy,
	operatorPub string,
	signDigest func(context.Context, []byte) ([]byte, error),
) (store.OperatorPolicyRecord, error) {
	return spendpolicy.ApprovePolicyPresetSigned(ctx, db, pol, preset, domains, approvedBy, aiPol, operatorPub, signDigest)
}

func PolicyProfileDefaults(tier string) (PolicyProfile, bool) {
	return spendpolicy.PolicyProfileDefaults(tier)
}

func ApprovePolicyCustom(
	db *store.DB,
	pol *Policy,
	preset string,
	domains []string,
	limits *PolicyLimitsOverride,
	approvedBy string,
	aiPol *AIMatchingPolicy,
) (store.OperatorPolicyRecord, error) {
	return spendpolicy.ApprovePolicyCustom(db, pol, preset, domains, limits, approvedBy, aiPol)
}

func ApprovePolicyPresetWithAI(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string, aiPol *AIMatchingPolicy) (store.OperatorPolicyRecord, error) {
	return spendpolicy.ApprovePolicyPresetWithAI(db, pol, preset, domains, approvedBy, aiPol)
}

func SyncPolicyFromDB(db *store.DB, pol *Policy) error {
	return spendpolicy.SyncPolicyFromDB(db, pol)
}
