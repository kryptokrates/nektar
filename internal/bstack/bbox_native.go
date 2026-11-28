package bstack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bbox/boxrec"
	boxreader "github.com/lightwebinc/bbox/reader"
	"github.com/lightwebinc/bcommon/carrier"

	"github.com/kryptokrates/nektar/internal/core"
)

const defaultBboxSuffix10 = "nektarbbox"

// BboxCarrierParams pins Lightweb's canonical bbox carrier parameters (`[1, "bbox message"]` key `"envelope"`, tag `bb\x02`).
var BboxCarrierParams = carrier.Params{
	Derivation: boxrec.EnvelopeDerivation,
	FundingTag: boxrec.TagFunding,
	ValidatePayload: func(b []byte) error {
		env, err := boxrec.DecodeEnvelope(b)
		if err != nil {
			return err
		}
		_, err = boxrec.CheckContent(env)
		return err
	},
}

type nativeBboxPlainPayload struct {
	AADHex     string `json:"aadHex"`
	PayloadB64 string `json:"payloadB64"`
}

// NormalizeBboxOffice normalizes a configured office string into Lightweb's strict `<name>_<10letters>`
// office grammar (`boxrec.CheckOffice`). Already-compliant office identifiers are returned unchanged.
func NormalizeBboxOffice(office string) string {
	s := strings.ToLower(strings.TrimSpace(office))
	if s == "" {
		s = "default"
	}
	if boxrec.CheckOffice(s) == nil {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < boxrec.MaxOfficeName; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		}
	}
	prefix := strings.Trim(b.String(), "-")
	if prefix == "" {
		prefix = "nektar"
	}
	cand := prefix + "_" + defaultBboxSuffix10
	if boxrec.CheckOffice(cand) == nil {
		return cand
	}
	return "nektar_" + defaultBboxSuffix10
}

// NormalizeBboxBox normalizes a message kind into a valid `boxrec.CheckBox` box name.
func NormalizeBboxBox(box string) string {
	s := strings.ToLower(strings.TrimSpace(box))
	if s == "" {
		return "market"
	}
	if boxrec.CheckBox(s) == nil {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < boxrec.MaxBox; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" || boxrec.CheckBox(out) != nil {
		return "market"
	}
	return out
}

// SealNativeBboxEnvelope seals plaintext into a canonical Lightweb BRC-169 + BRC-78 `boxrec.Envelope`
// (signed under `[1, "bbox message"]` key `"signature"`, encrypted under `[2, "message encryption"]`,
// and validated via `boxrec.CheckContent` + `env.Encode()`).
func SealNativeBboxEnvelope(
	ctx context.Context,
	w boxrec.Sealer,
	office, box, fromPubHex, toPubHex string,
	aad, plaintext []byte,
	created, expires uint64,
) (*boxrec.Envelope, []byte, error) {
	if w == nil {
		return nil, nil, errors.New("bbox native: sealer wallet required")
	}
	_, fromBytes, err := core.ParseStrictCompressedPubKey(fromPubHex)
	if err != nil {
		return nil, nil, fmt.Errorf("bbox native: invalid sender pubkey: %w", err)
	}
	_, toBytes, err := core.ParseStrictCompressedPubKey(toPubHex)
	if err != nil {
		return nil, nil, fmt.Errorf("bbox native: invalid recipient pubkey: %w", err)
	}
	if created == 0 {
		created = 1
	}
	if expires != 0 && expires <= created {
		expires = created + 3600
	}

	wrapped, err := json.Marshal(nativeBboxPlainPayload{
		AADHex:     hex.EncodeToString(aad),
		PayloadB64: base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return nil, nil, err
	}
	plainDoc := &boxrec.Object{}
	plainDoc.Set(boxrec.PlainBody, string(wrapped))
	canonPlain, err := boxrec.EncodePlaintext(plainDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("bbox native: encode plaintext: %w", err)
	}

	contentJSON, err := boxrec.SealEnvelope(ctx, w, Originator, fromBytes, toBytes, canonPlain, created)
	if err != nil {
		return nil, nil, fmt.Errorf("bbox native: seal BRC-169 envelope: %w", err)
	}
	if len(contentJSON) > boxrec.MaxContent {
		return nil, nil, boxrec.ErrTooLarge
	}
	env := &boxrec.Envelope{
		Office:  NormalizeBboxOffice(office),
		To:      toBytes,
		Box:     NormalizeBboxBox(box),
		From:    fromBytes,
		Created: created,
		Expires: expires,
		Content: contentJSON,
	}
	if _, err := boxrec.CheckContent(env); err != nil {
		return nil, nil, fmt.Errorf("bbox native: check sealed content: %w", err)
	}
	envCBOR, err := env.Encode()
	if err != nil {
		return nil, nil, err
	}
	return env, envCBOR, nil
}

// OpenNativeBboxContent verifies a Lightweb BRC-169 + BRC-78 `boxrec` content JSON payload via
// `boxrec.CheckContent`, decrypts it via `boxrec.Open`, validates canonical `boxrec.ParsePlaintext`,
// and verifies the exact length-framed AAD binding.
func OpenNativeBboxContent(
	ctx context.Context,
	w boxrec.Decrypter,
	office, box, fromPubHex, toPubHex string,
	created, expires uint64,
	aad, contentJSON []byte,
) ([]byte, error) {
	if w == nil {
		return nil, errors.New("bbox native: decrypter wallet required")
	}
	_, fromBytes, err := core.ParseStrictCompressedPubKey(fromPubHex)
	if err != nil {
		return nil, err
	}
	_, toBytes, err := core.ParseStrictCompressedPubKey(toPubHex)
	if err != nil {
		return nil, err
	}
	if created == 0 {
		created = 1
	}
	env := &boxrec.Envelope{
		Office:  NormalizeBboxOffice(office),
		To:      toBytes,
		Box:     NormalizeBboxBox(box),
		From:    fromBytes,
		Created: created,
		Expires: expires,
		Content: contentJSON,
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	checked, err := boxrec.CheckContent(env)
	if err != nil {
		return nil, err
	}
	canonPlain, err := boxrec.Open(ctx, w, Originator, checked, fromBytes)
	if err != nil {
		return nil, err
	}
	pt, err := boxrec.ParsePlaintext(canonPlain)
	if err != nil {
		return nil, err
	}
	if pt.Body == nil {
		return nil, errors.New("bbox native: missing plaintext body")
	}
	var wrapped nativeBboxPlainPayload
	if err := json.Unmarshal([]byte(*pt.Body), &wrapped); err != nil {
		return nil, fmt.Errorf("bbox native: decode wrapped payload: %w", err)
	}
	gotAAD, err := hex.DecodeString(wrapped.AADHex)
	if err != nil || !bytes.Equal(gotAAD, aad) {
		return nil, errors.New("bbox native: AAD binding mismatch")
	}
	return base64.StdEncoding.DecodeString(wrapped.PayloadB64)
}

// IsNativeBboxContent reports whether raw JSON is a top-level object carrying Lightweb's BRC-169 `metanetHandles` key.
func IsNativeBboxContent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return false
	}
	var probe struct {
		MetanetHandles json.RawMessage `json:"metanetHandles"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return false
	}
	return len(bytes.TrimSpace(probe.MetanetHandles)) > 0
}

// SealBboxMessageBody seals a counterparty message using Lightweb's native BRC-169 + BRC-78 `boxrec.SealEnvelope`
// whenever `w != nil` and the sealed BRC-169 content fits within `boxrec.MaxContent` (16 KiB),
// falling back to v2 `SealBboxBody` only when oversized (e.g. large multi-ancestor ProofBundle).
func SealBboxMessageBody(
	ctx context.Context,
	w boxrec.Sealer,
	office, kind, fromPubHex, toPubHex string,
	createdUnix int64,
	aad, plaintext []byte,
) (json.RawMessage, error) {
	if w != nil && createdUnix > 0 {
		created := uint64(createdUnix)
		if env, _, err := SealNativeBboxEnvelope(ctx, w, office, kind, fromPubHex, toPubHex, aad, plaintext, created, created+3600); err == nil {
			return json.RawMessage(env.Content), nil
		}
	}
	return SealBboxBody(toPubHex, aad, plaintext)
}

// OpenBboxMessageBody verifies and opens either a native Lightweb BRC-169 + BRC-78 `boxrec` envelope
// or a backward-compatible v2 `BboxSealedBody` envelope.
func OpenBboxMessageBody(
	ctx context.Context,
	w boxrec.Decrypter,
	opener BboxOpener,
	office, kind, fromPubHex, toPubHex string,
	createdUnix int64,
	aad []byte,
	raw json.RawMessage,
) ([]byte, error) {
	return OpenBboxMessageBodyWithExpiry(ctx, w, opener, office, kind, fromPubHex, toPubHex, createdUnix, 0, aad, raw)
}

// OpenBboxMessageBodyWithExpiry verifies and opens a native BRC-169 or v2 BboxSealedBody envelope
// preserving explicit sender-chosen expiresUnix (0 = no expiry override).
func OpenBboxMessageBodyWithExpiry(
	ctx context.Context,
	w boxrec.Decrypter,
	opener BboxOpener,
	office, kind, fromPubHex, toPubHex string,
	createdUnix, expiresUnix int64,
	aad []byte,
	raw json.RawMessage,
) ([]byte, error) {
	if IsNativeBboxContent(raw) {
		if createdUnix <= 0 {
			return nil, errors.New("bbox native: positive message timestamp required")
		}
		var expires uint64
		if expiresUnix > 0 {
			expires = uint64(expiresUnix)
		}
		return OpenNativeBboxContent(ctx, w, office, kind, fromPubHex, toPubHex, uint64(createdUnix), expires, aad, raw)
	}
	return OpenBboxBodyWithSigner(opener, aad, raw)
}

// MintBboxCarrier mints an unmineable `tm_bbox_<office>` PushDrop carrier spending fundTx:fundVout.
func MintBboxCarrier(
	ctx context.Context,
	w sdkwallet.Interface,
	env *boxrec.Envelope,
	fundTx *transaction.Transaction,
	fundVout uint32,
) ([]byte, string, error) {
	if w == nil || env == nil || fundTx == nil {
		return nil, "", errors.New("bbox native: wallet, envelope, and funding tx required")
	}
	envCBOR, err := env.Encode()
	if err != nil {
		return nil, "", err
	}
	carrierTx, err := carrier.Mint(ctx, w, Originator, BboxCarrierParams, envCBOR, fundTx, fundVout)
	if err != nil {
		return nil, "", err
	}
	if len(carrierTx.Inputs) > 0 && carrierTx.Inputs[0].SourceTransaction == nil {
		carrierTx.Inputs[0].SourceTransaction = fundTx
	}
	beef, err := carrierTx.AtomicBEEF(false)
	if err != nil {
		return nil, "", err
	}
	return beef, carrierTx.TxID().String(), nil
}

// VerifyBboxCarrier verifies an answered `ls_bbox` carrier AtomicBEEF against witnessed headers via `bbox/reader.Verify`.
func VerifyBboxCarrier(
	ctx context.Context,
	beef []byte,
	office string,
	toPub []byte,
	tracker chaintracker.ChainTracker,
) (*boxreader.Item, error) {
	return boxreader.Verify(ctx, beef, NormalizeBboxOffice(office), boxrec.TxEnvelope, toPub, tracker)
}
