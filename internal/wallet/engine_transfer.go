package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	"github.com/lightwebinc/bcommon/guard"
)

// -----------------------------------------------------------------------------
// High-Level Transfer Operators: Send
// -----------------------------------------------------------------------------

// Send sends BSV to a P2PKH address OR a BRC-100 identity public key (BRC-29).
func (e *Engine) Send(ctx context.Context, args SendArgs) (*SendResult, error) {
	if args.Satoshis == 0 {
		return nil, fmt.Errorf("%w: send satoshis must be > 0", ErrInvalidArgs)
	}
	if (args.ToAddress == "" && args.ToIdentityKey == "") || (args.ToAddress != "" && args.ToIdentityKey != "") {
		return nil, fmt.Errorf("%w: specify exactly one of toAddress or toIdentityKey", ErrInvalidArgs)
	}
	if e.fastLane != nil && !args.NoSend {
		if res, err := e.fastLane.SendFast(ctx, args); err == nil {
			return res, nil
		} else if err != errFastLaneUnavailable {
			return nil, err
		}
	}

	var lockScript *script.Script
	var prefixB64, suffixB64, senderHex string
	var err error
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, aErr)
		}
		lockScript, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else {
		prefixB64 = strings.TrimSpace(args.DerivationPrefix)
		suffixB64 = strings.TrimSpace(args.DerivationSuffix)
		if prefixB64 == "" {
			var preRaw [16]byte
			if _, err := rand.Read(preRaw[:]); err != nil {
				return nil, fmt.Errorf("wallet: crypto/rand failed: %w", err)
			}
			prefixB64 = base64.StdEncoding.EncodeToString(preRaw[:])
		}
		if suffixB64 == "" {
			var sufRaw [16]byte
			if _, err := rand.Read(sufRaw[:]); err != nil {
				return nil, fmt.Errorf("wallet: crypto/rand failed: %w", err)
			}
			suffixB64 = base64.StdEncoding.EncodeToString(sufRaw[:])
		}
		senderHex = e.IdentityPubHex()

		recipPub, pErr := guard.ParsePubKeyHex(strings.TrimSpace(args.ToIdentityKey))
		if pErr != nil {
			return nil, fmt.Errorf("%w: invalid toIdentityKey: %v", ErrInvalidArgs, pErr)
		}
		if !e.cfg.Mainnet {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub, brc29.WithTestNet())
		} else {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub)
		}
		if err != nil {
			return nil, fmt.Errorf("wallet: derive BRC-29 payment script: %w", err)
		}
	}

	desc := ensureDesc(args.Description, "nektar wallet send")
	noSend := args.NoSend
	acceptDelayed := args.AcceptDelayed
	randomize := false
	caRes, err := e.createActionGuarded(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      args.Labels,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *lockScript,
			Satoshis:          args.Satoshis,
			OutputDescription: desc,
		}},
		Options: &sdkwallet.CreateActionOptions{
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, e.cfg.Originator, args.OverrideGuard, false)
	if err != nil {
		return nil, err
	}

	txid := caRes.Txid.String()
	var parsed *transaction.Transaction
	if len(caRes.Tx) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, txid)
	}
	outIdx := uint32(0)
	inputCount := 0
	var feeSats uint64
	if parsed != nil {
		inputCount = len(parsed.Inputs)
		feeSats = ComputeTxFee(parsed)
		for idx, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) && o.Satoshis == args.Satoshis {
				outIdx = uint32(idx)
				break
			}
		}
	}
	return &SendResult{
		TxID:              txid,
		Satoshis:          args.Satoshis,
		FeeSats:           feeSats,
		InputCount:        inputCount,
		BEEF:              caRes.Tx,
		BEEFHex:           hex.EncodeToString(caRes.Tx),
		Tx:                parsed,
		DerivationPrefix:  prefixB64,
		DerivationSuffix:  suffixB64,
		SenderIdentityKey: senderHex,
		OutputIndex:       outIdx,
	}, nil
}
