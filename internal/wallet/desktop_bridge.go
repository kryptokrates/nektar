package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/wirewallet"
)

const DefaultDesktopWireURL = "http://127.0.0.1:3321"

// dialWireWallet can be overridden in unit tests to simulate crashes between CreateAction and InternalizeAction.
var dialWireWallet = func(originator, url string, timeout time.Duration) (sdkwallet.Interface, error) {
	return DialDesktopWallet(context.Background(), originator, url, timeout)
}

// DesktopProbeStatus summarizes whether a local BRC-100 desktop wallet (BSV Desktop / Metanet Desktop)
// is currently listening on loopback (default http://127.0.0.1:3321).
type DesktopProbeStatus struct {
	Available   bool   `json:"available"`
	WireURL     string `json:"wireUrl"`
	Transport   string `json:"transport,omitempty"`
	Network     string `json:"network,omitempty"`
	IdentityPub string `json:"identityPub,omitempty"`
	Error       string `json:"error,omitempty"`
}

// isLoopbackTCPReachable performs a fast non-blocking TCP connect check (<= timeout) against wireURL
// so offline BSV Desktop instances fail fast instead of stalling Monitor for 15s.
func isLoopbackTCPReachable(ctx context.Context, wireURL string, timeout time.Duration) bool {
	u, err := url.Parse(wireURL)
	if err != nil || u.Host == "" {
		return false
	}
	hostPort := u.Host
	if _, _, splitErr := net.SplitHostPort(hostPort); splitErr != nil {
		if strings.EqualFold(u.Scheme, "https") {
			hostPort = net.JoinHostPort(hostPort, "443")
		} else {
			hostPort = net.JoinHostPort(hostPort, "80")
		}
	}
	if timeout <= 0 {
		timeout = 250 * time.Millisecond
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, dErr := dialer.DialContext(ctx, "tcp", hostPort)
	if dErr != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeDesktopWallet performs a fast non-blocking check for a local BRC-100 wallet on wireURL.
// It checks TCP reachability first (<= 250ms) so offline machines never pay an HTTP timeout penalty.
func ProbeDesktopWallet(ctx context.Context, wireURL string) DesktopProbeStatus {
	if strings.TrimSpace(wireURL) == "" {
		wireURL = DefaultDesktopWireURL
	}
	st := DesktopProbeStatus{WireURL: wireURL}
	if err := wirewallet.CheckURL(wireURL); err != nil {
		st.Error = err.Error()
		return st
	}
	if u, err := url.Parse(wireURL); err != nil || u.Host == "" {
		st.Error = "invalid wire URL"
		return st
	}
	if !isLoopbackTCPReachable(ctx, wireURL, 250*time.Millisecond) {
		return st
	}

	pCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	w, err := dialWireWallet(DefaultDesktopOriginator, wireURL, 2*time.Second)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if _, ok := w.(*DesktopJSONClient); ok {
		st.Transport = string(DesktopTransportHTTPJSON)
	} else {
		st.Transport = string(DesktopTransportBinary)
	}
	netRes, err := w.GetNetwork(pCtx, nil, DefaultDesktopOriginator)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Available = true
	if netRes != nil {
		st.Network = string(netRes.Network)
	}
	if idPub, idErr := wirewallet.IdentityKeyOf(pCtx, w, DefaultDesktopOriginator); idErr == nil && idPub != nil {
		st.IdentityPub = hex.EncodeToString(idPub.Compressed())
	}
	return st
}

// DesktopFundResult summarizes a 1-click BRC-29 funding transfer from BSV Desktop into Nektar.
type DesktopFundResult struct {
	TxID             string `json:"txid"`
	Satoshis         uint64 `json:"satoshis"`
	DesktopIdentity  string `json:"desktopIdentity"`
	NektarIdentity   string `json:"nektarIdentity"`
	DerivationPrefix string `json:"derivationPrefix"`
	DerivationSuffix string `json:"derivationSuffix"`
	Internalized     bool   `json:"internalized"`
}

// DesktopSweepResult summarizes a 1-click BRC-29 sweep from Nektar back into BSV Desktop.
type DesktopSweepResult struct {
	TxIDs           []string `json:"txids"`
	SweptSats       uint64   `json:"sweptSats"`
	TotalFeeSats    uint64   `json:"totalFeeSats"`
	InputsSwept     int      `json:"inputsSwept"`
	DesktopIdentity string   `json:"desktopIdentity"`
	Internalized    int      `json:"internalized"`
}

// FundFromWireWallet dials a local BRC-100 wallet (BSV Desktop / Metanet Desktop / external go-wallet-toolbox)
// over loopback binary wire, requests one BRC-29 payment output to target's identity key, and immediately
// internalizes the resulting AtomicBEEF into target so the balance is verified and spendable on arrival.
func FundFromWireWallet(ctx context.Context, wireURL string, amountSats uint64, target *Engine) (*DesktopFundResult, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: target embedded wallet is required", ErrInvalidArgs)
	}
	if amountSats == 0 {
		return nil, fmt.Errorf("%w: amountSats must be > 0", ErrInvalidArgs)
	}
	if wireURL == "" {
		wireURL = DefaultDesktopWireURL
	}
	if err := wirewallet.CheckURL(wireURL); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: %w", err)
	}
	desktop, err := dialWireWallet(target.cfg.Originator, wireURL, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: dial %s: %w", wireURL, err)
	}

	// Refuse any mainnet/testnet mismatch before requesting a spend!
	if err := checkMatchingNetwork(ctx, desktop, target.cfg.Mainnet, target.cfg.Originator); err != nil {
		return nil, err
	}
	desktopID, err := wirewallet.IdentityKeyOf(ctx, desktop, target.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: get desktop identity: %w", err)
	}
	desktopHex := hex.EncodeToString(desktopID.Compressed())

	// Generate fresh BRC-29 derivation nonce pair and ask desktop wallet to derive recipient's payment pubkey.
	var prefixRaw, suffixRaw [16]byte
	if _, err := rand.Read(prefixRaw[:]); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: crypto/rand failed: %w", err)
	}
	if _, err := rand.Read(suffixRaw[:]); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: crypto/rand failed: %w", err)
	}
	prefixB64 := base64.StdEncoding.EncodeToString(prefixRaw[:])
	suffixB64 := base64.StdEncoding.EncodeToString(suffixRaw[:])

	forSelf := false
	pubRes, err := desktop.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: bwallet.PaymentProtocol,
			KeyID:      bwallet.PaymentKeyID(prefixB64, suffixB64),
			Counterparty: sdkwallet.Counterparty{
				Type:         sdkwallet.CounterpartyTypeOther,
				Counterparty: target.IdentityKey(),
			},
		},
		ForSelf: &forSelf,
	}, target.cfg.Originator)
	if err != nil || pubRes == nil || pubRes.PublicKey == nil {
		return nil, fmt.Errorf("wallet desktop bridge: derive BRC-29 payment key: %w", err)
	}
	addr, err := script.NewAddressFromPublicKey(pubRes.PublicKey, target.cfg.Mainnet)
	if err != nil {
		return nil, err
	}
	lockScript, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, err
	}

	// Persist (prefix, suffix, counterparty, lockingScript) BEFORE desktop.CreateAction so a crash
	// between broadcast and InternalizeAction can always recover the funds on restart!
	pendingID, err := target.SavePendingBRC29(ctx, PendingBRC29Record{
		Direction:        "fund",
		WireURL:          wireURL,
		CounterpartyPub:  desktopHex,
		DerivationPrefix: prefixB64,
		DerivationSuffix: suffixB64,
		LockingScriptHex: hex.EncodeToString(*lockScript),
		Satoshis:         amountSats,
	})
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: journal pending BRC-29 fund: %w", err)
	}

	// Ask BSV Desktop to fund, sign, and broadcast (AcceptDelayedBroadcast=false so AtomicBEEF is live).
	acceptDelayed := false
	randomize := false
	fundMeta := ActionFundAgentWallet(amountSats)
	actRes, err := desktop.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: fundMeta.Description,
		Labels:      fundMeta.Labels,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *lockScript,
			Satoshis:          amountSats,
			OutputDescription: ActionFundAgentWalletOutput(amountSats).Description,
		}},
		Options: &sdkwallet.CreateActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, target.cfg.Originator)
	if err != nil {
		if !IsUncertainBroadcastError(err) {
			// Definite pre-broadcast refusal (e.g. user cancelled prompt, insufficient funds): remove pending row.
			_ = target.DeletePendingBRC29(ctx, pendingID)
		}
		return nil, fmt.Errorf("wallet desktop bridge: desktop CreateAction: %w", err)
	}
	txidStr := actRes.Txid.String()
	if len(actRes.Tx) == 0 {
		// Persist the broadcast txid BEFORE returning so ReconcilePendingBRC29 can recover via chain BEEF!
		if uErr := target.UpdatePendingBRC29Tx(ctx, pendingID, txidStr, 0, nil, amountSats); uErr != nil {
			target.logf("wallet desktop bridge: persist empty-BEEF txid %s (pending %d): %v", txidStr, pendingID, uErr)
		}
		return nil, fmt.Errorf("wallet desktop bridge: desktop CreateAction returned empty AtomicBEEF (txid %s persisted for recovery)", txidStr)
	}

	// Locate exact output index matching our derived locking script (fail closed on parse/match failure; never default outIdx=0).
	_, parsedTx, _, pErr := guard.ParseBEEF(actRes.Tx, guard.DefaultBound)
	if pErr != nil || parsedTx == nil {
		if uErr := target.UpdatePendingBRC29Tx(ctx, pendingID, txidStr, 0, actRes.Tx, amountSats); uErr != nil {
			target.logf("wallet desktop bridge: persist unparsed BEEF %s (pending %d): %v", txidStr, pendingID, uErr)
		}
		return nil, fmt.Errorf("wallet desktop bridge: parse desktop AtomicBEEF for %s: %v", txidStr, pErr)
	}
	var outIdx uint32
	matched := false
	for idx, o := range parsedTx.Outputs {
		if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) && o.Satoshis == amountSats {
			outIdx = uint32(idx)
			matched = true
			break
		}
	}
	if !matched {
		if uErr := target.UpdatePendingBRC29Tx(ctx, pendingID, txidStr, 0, actRes.Tx, amountSats); uErr != nil {
			target.logf("wallet desktop bridge: persist unmatched BEEF %s (pending %d): %v", txidStr, pendingID, uErr)
		}
		return nil, fmt.Errorf("wallet desktop bridge: broadcast tx %s missing expected BRC-29 output script", txidStr)
	}
	if uErr := target.UpdatePendingBRC29Tx(ctx, pendingID, txidStr, outIdx, actRes.Tx, amountSats); uErr != nil {
		target.logf("wallet desktop bridge: update pending BRC-29 %d (%s): %v", pendingID, txidStr, uErr)
	}

	// Immediately internalize AtomicBEEF + BRC-29 remittance into target Engine!
	intRes, err := target.InternalizeAction(WithIncomingInternalize(ctx), sdkwallet.InternalizeActionArgs{
		Tx:          actRes.Tx,
		Description: fmt.Sprintf("Funded %d sats from BSV Desktop", amountSats),
		Labels:      []string{"desktop-fund", "brc29"},
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: outIdx,
			Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdkwallet.Payment{
				DerivationPrefix:  prefixRaw[:],
				DerivationSuffix:  suffixRaw[:],
				SenderIdentityKey: desktopID,
			},
		}},
	}, target.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: internalize into Nektar (txid %s): %w", txidStr, err)
	}
	if intRes == nil || !intRes.Accepted {
		return nil, fmt.Errorf("wallet desktop bridge: Nektar rejected internalization of desktop fund tx %s", txidStr)
	}
	if mErr := target.MarkPendingBRC29Done(ctx, pendingID); mErr != nil {
		target.logf("wallet desktop bridge: mark pending BRC-29 fund %d done (%s): %v", pendingID, txidStr, mErr)
	}
	target.fuel.Wake()

	return &DesktopFundResult{
		TxID:             txidStr,
		Satoshis:         amountSats,
		DesktopIdentity:  desktopHex,
		NektarIdentity:   target.IdentityPubHex(),
		DerivationPrefix: prefixB64,
		DerivationSuffix: suffixB64,
		Internalized:     true,
	}, nil
}

// SweepToWireWallet sweeps 100% of spendable funds from source Engine to fresh BRC-29 keys
// owned by the local BSV Desktop / BRC-100 wallet and calls desktop.InternalizeAction for every chunk
// so the swept funds appear instantly in the user's desktop wallet!
func SweepToWireWallet(ctx context.Context, wireURL string, source *Engine, overrideGuard bool) (*DesktopSweepResult, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: source embedded wallet is required", ErrInvalidArgs)
	}
	if wireURL == "" {
		wireURL = DefaultDesktopWireURL
	}
	if err := wirewallet.CheckURL(wireURL); err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: %w", err)
	}
	desktop, err := dialWireWallet(source.cfg.Originator, wireURL, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: dial %s: %w", wireURL, err)
	}
	if err := checkMatchingNetwork(ctx, desktop, source.cfg.Mainnet, source.cfg.Originator); err != nil {
		return nil, err
	}
	desktopID, err := wirewallet.IdentityKeyOf(ctx, desktop, source.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet desktop bridge: get desktop identity: %w", err)
	}
	desktopHex := hex.EncodeToString(desktopID.Compressed())

	sweepRes, err := source.Sweep(ctx, SweepArgs{
		ToIdentityKey: desktopHex,
		WireURL:       wireURL,
		Description:   ActionSweepAgentWallet().Description,
		MaxPerTx:      DefaultSweepChunkInputs,
		OverrideGuard: overrideGuard,
	})
	if err != nil {
		return nil, err
	}

	out := &DesktopSweepResult{
		TxIDs:           sweepRes.TxIDs,
		SweptSats:       sweepRes.SweptSats,
		TotalFeeSats:    sweepRes.TotalFeeSats,
		InputsSwept:     sweepRes.InputsSwept,
		DesktopIdentity: desktopHex,
	}
	for i, rem := range sweepRes.Remittances {
		if i >= len(sweepRes.BEEFs) || len(sweepRes.BEEFs[i]) == 0 {
			return out, fmt.Errorf("wallet desktop bridge: missing AtomicBEEF for sweep chunk %d (txid %s)", i, rem.TxID)
		}
		preRaw, err1 := base64.StdEncoding.DecodeString(rem.DerivationPrefix)
		sufRaw, err2 := base64.StdEncoding.DecodeString(rem.DerivationSuffix)
		if err1 != nil || err2 != nil {
			return out, fmt.Errorf("wallet desktop bridge: invalid BRC-29 remittance encoding for sweep %s", rem.TxID)
		}
		recvMeta := ActionReceiveSweptFunds()
		intRes, iErr := desktop.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
			Tx:          sweepRes.BEEFs[i],
			Description: recvMeta.Description,
			Labels:      recvMeta.Labels,
			Outputs: []sdkwallet.InternalizeOutput{{
				OutputIndex: rem.OutputIndex,
				Protocol:    sdkwallet.InternalizeProtocolWalletPayment,
				PaymentRemittance: &sdkwallet.Payment{
					DerivationPrefix:  preRaw,
					DerivationSuffix:  sufRaw,
					SenderIdentityKey: source.IdentityKey(),
				},
			}},
		}, source.cfg.Originator)
		if iErr != nil {
			return out, fmt.Errorf("wallet desktop bridge: desktop InternalizeAction for sweep %s: %w", rem.TxID, iErr)
		}
		if intRes == nil || !intRes.Accepted {
			return out, fmt.Errorf("wallet desktop bridge: desktop rejected internalization for sweep %s", rem.TxID)
		}
		if mErr := source.MarkPendingBRC29DoneByTx(ctx, rem.TxID, rem.OutputIndex); mErr != nil {
			source.logf("wallet desktop bridge: mark pending BRC-29 sweep done (%s:%d): %v", rem.TxID, rem.OutputIndex, mErr)
		}
		out.Internalized++
	}
	return out, nil
}

func checkMatchingNetwork(ctx context.Context, remote sdkwallet.Interface, localMainnet bool, originator string) error {
	netRes, err := remote.GetNetwork(ctx, nil, originator)
	if err != nil {
		return fmt.Errorf("wallet desktop bridge: get remote network: %w", err)
	}
	want := sdkwallet.NetworkTestnet
	if localMainnet {
		want = sdkwallet.NetworkMainnet
	}
	if netRes == nil || netRes.Network != want {
		got := sdkwallet.Network("")
		if netRes != nil {
			got = netRes.Network
		}
		return fmt.Errorf("%w: Nektar is on %q, desktop wallet is on %q", ErrNetworkMismatch, want, got)
	}
	return nil
}
