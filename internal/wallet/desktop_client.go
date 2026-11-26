package wallet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/wirewallet"
)

// DefaultDesktopOriginator is the canonical app domain Nektar presents to BSV Desktop
// so permission dialogs and Connected Apps resolve https://nektar.run/manifest.json and favicon.ico.
const DefaultDesktopOriginator = "nektar.run"

// DesktopTransport identifies the BRC-100 transport spoken by a local desktop wallet.
type DesktopTransport string

const (
	DesktopTransportHTTPJSON DesktopTransport = "http-json"
	DesktopTransportBinary   DesktopTransport = "binary-wire"
)

// NormalizeDesktopOrigin converts an originator string (such as "nektar", "nektar.run",
// or "http://localhost:18420") into a valid Origin URL header and bare Originator domain
// accepted by BSV Desktop's strict URL parser on :3321.
func NormalizeDesktopOrigin(originator string) (originURL, originatorHost string) {
	raw := strings.TrimSpace(originator)
	if raw == "" || strings.EqualFold(raw, "nektar") {
		return "https://" + DefaultDesktopOriginator, DefaultDesktopOriginator
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host, u.Host
		}
		return "https://" + DefaultDesktopOriginator, DefaultDesktopOriginator
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "localhost") || strings.HasPrefix(lower, "127.0.0.1") {
		return "http://" + raw, raw
	}
	if strings.Contains(raw, ".") {
		return "https://" + raw, raw
	}
	return "https://" + DefaultDesktopOriginator, DefaultDesktopOriginator
}

// DialDesktopWallet connects to a loopback BRC-100 wallet (BSV Desktop / Metanet Desktop on :3321
// or a binary-wire wallet such as wirewallet.Serve on :3301) and automatically selects HTTP-JSON
// or binary wire based on a fast /getNetwork transport probe.
func DialDesktopWallet(ctx context.Context, originator, baseURL string, timeout time.Duration) (sdkwallet.Interface, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultDesktopWireURL
	}
	if err := wirewallet.CheckURL(baseURL); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}

	probeTimeout := 2 * time.Second
	if timeout < probeTimeout {
		probeTimeout = timeout
	}
	pCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if isHTTPJSONDesktopWallet(pCtx, originator, baseURL, probeTimeout) {
		return NewDesktopJSONClient(originator, baseURL, &http.Client{Timeout: timeout}), nil
	}
	return wirewallet.Dial(originator, baseURL, timeout)
}

func isHTTPJSONDesktopWallet(ctx context.Context, originator, baseURL string, timeout time.Duration) bool {
	originURL, origHost := NormalizeDesktopOrigin(originator)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/getNetwork", bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", originURL)
	req.Header.Set("Originator", origHost)

	cli := &http.Client{Timeout: timeout}
	resp, err := cli.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// DesktopJSONClient implements sdkwallet.Interface over BSV Desktop's BRC-100 HTTP-JSON
// substrate (default http://127.0.0.1:3321), fixing go-sdk's value-receiver Protocol/Counterparty
// marshaling, optional-field omitempty rules, and BytesList BEEF encoding against TS wallet-toolbox.
type DesktopJSONClient struct {
	baseURL    string
	httpClient *http.Client
	originator string
}

var _ sdkwallet.Interface = (*DesktopJSONClient)(nil)

// NewDesktopJSONClient creates a BRC-100 HTTP-JSON client targeting a loopback desktop wallet.
func NewDesktopJSONClient(originator, baseURL string, httpClient *http.Client) *DesktopJSONClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultDesktopWireURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &DesktopJSONClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		originator: originator,
	}
}

func (c *DesktopJSONClient) call(ctx context.Context, method string, payload any, originator string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reqBody []byte
	var err error
	if payload == nil {
		reqBody = []byte("{}")
	} else {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("desktop wallet json marshal %s: %w", method, err)
		}
		if len(reqBody) == 0 || bytes.Equal(reqBody, []byte("null")) {
			reqBody = []byte("{}")
		}
	}

	orig := strings.TrimSpace(originator)
	if orig == "" {
		orig = c.originator
	}
	originURL, origHost := NormalizeDesktopOrigin(orig)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("desktop wallet request %s: %w", method, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", originURL)
	req.Header.Set("Originator", origHost)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("desktop wallet %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("desktop wallet read %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("desktop wallet %s: HTTP %d: %s", method, resp.StatusCode, msg)
	}
	return data, nil
}

// flexBytes decodes either a BRC-100 JSON byte array ([1, 0, ...]) or a base64 string.
type flexBytes []byte

func (f *flexBytes) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = nil
		return nil
	}
	if trimmed[0] == '[' {
		var bl sdkwallet.BytesList
		if err := json.Unmarshal(trimmed, &bl); err != nil {
			return err
		}
		*f = flexBytes(bl)
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		if s == "" {
			*f = nil
			return nil
		}
		dec, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return err
		}
		*f = flexBytes(dec)
		return nil
	}
	return fmt.Errorf("flexBytes: unsupported JSON value %s", string(trimmed))
}

type desktopGetPublicKeyWire struct {
	ProtocolID       *sdkwallet.Protocol     `json:"protocolID,omitempty"`
	KeyID            string                  `json:"keyID,omitempty"`
	Counterparty     *sdkwallet.Counterparty `json:"counterparty,omitempty"`
	Privileged       bool                    `json:"privileged,omitempty"`
	PrivilegedReason string                  `json:"privilegedReason,omitempty"`
	SeekPermission   bool                    `json:"seekPermission,omitempty"`
	IdentityKey      bool                    `json:"identityKey,omitempty"`
	ForSelf          *bool                   `json:"forSelf,omitempty"`
}

func (c *DesktopJSONClient) GetPublicKey(ctx context.Context, args sdkwallet.GetPublicKeyArgs, originator string) (*sdkwallet.GetPublicKeyResult, error) {
	wire := desktopGetPublicKeyWire{
		Privileged:       args.Privileged,
		PrivilegedReason: args.PrivilegedReason,
		SeekPermission:   args.SeekPermission,
		IdentityKey:      args.IdentityKey,
		ForSelf:          args.ForSelf,
	}
	if !args.IdentityKey {
		proto := args.ProtocolID
		wire.ProtocolID = &proto
		wire.KeyID = args.KeyID
		if args.Counterparty.Type != sdkwallet.CounterpartyUninitialized {
			cp := args.Counterparty
			wire.Counterparty = &cp
		}
	}
	data, err := c.call(ctx, "getPublicKey", &wire, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.GetPublicKeyResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet getPublicKey decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) Encrypt(ctx context.Context, args sdkwallet.EncryptArgs, originator string) (*sdkwallet.EncryptResult, error) {
	data, err := c.call(ctx, "encrypt", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.EncryptResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet encrypt decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) Decrypt(ctx context.Context, args sdkwallet.DecryptArgs, originator string) (*sdkwallet.DecryptResult, error) {
	data, err := c.call(ctx, "decrypt", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.DecryptResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet decrypt decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) CreateHMAC(ctx context.Context, args sdkwallet.CreateHMACArgs, originator string) (*sdkwallet.CreateHMACResult, error) {
	data, err := c.call(ctx, "createHmac", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.CreateHMACResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet createHmac decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) VerifyHMAC(ctx context.Context, args sdkwallet.VerifyHMACArgs, originator string) (*sdkwallet.VerifyHMACResult, error) {
	data, err := c.call(ctx, "verifyHmac", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.VerifyHMACResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet verifyHmac decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) CreateSignature(ctx context.Context, args sdkwallet.CreateSignatureArgs, originator string) (*sdkwallet.CreateSignatureResult, error) {
	data, err := c.call(ctx, "createSignature", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.CreateSignatureResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet createSignature decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) VerifySignature(ctx context.Context, args sdkwallet.VerifySignatureArgs, originator string) (*sdkwallet.VerifySignatureResult, error) {
	data, err := c.call(ctx, "verifySignature", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.VerifySignatureResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet verifySignature decode: %w", err)
	}
	return &res, nil
}

type desktopCreateActionArgsWire struct {
	Description string                         `json:"description"`
	InputBEEF   sdkwallet.BytesList            `json:"inputBEEF,omitempty"`
	Inputs      []sdkwallet.CreateActionInput  `json:"inputs,omitempty"`
	Outputs     []sdkwallet.CreateActionOutput `json:"outputs,omitempty"`
	LockTime    *uint32                        `json:"lockTime,omitempty"`
	Version     *uint32                        `json:"version,omitempty"`
	Labels      []string                       `json:"labels,omitempty"`
	Options     *sdkwallet.CreateActionOptions `json:"options,omitempty"`
}

type desktopSignableTxWire struct {
	Tx        flexBytes `json:"tx"`
	Reference flexBytes `json:"reference"`
}

type desktopCreateActionResultWire struct {
	Txid                chainhash.Hash             `json:"txid,omitempty"`
	Tx                  flexBytes                  `json:"tx,omitempty"`
	NoSendChange        []transaction.Outpoint     `json:"noSendChange,omitempty"`
	SendWithResults     []sdkwallet.SendWithResult `json:"sendWithResults,omitempty"`
	SignableTransaction *desktopSignableTxWire     `json:"signableTransaction,omitempty"`
}

func (c *DesktopJSONClient) CreateAction(ctx context.Context, args sdkwallet.CreateActionArgs, originator string) (*sdkwallet.CreateActionResult, error) {
	desc := NormalizeActionDescription(args.Description)
	outs := make([]sdkwallet.CreateActionOutput, len(args.Outputs))
	for i, o := range args.Outputs {
		o.OutputDescription = NormalizeActionDescription(o.OutputDescription)
		if o.OutputDescription == ActionPrefix+"Transaction" {
			o.OutputDescription = desc
		}
		outs[i] = o
	}
	ins := make([]sdkwallet.CreateActionInput, len(args.Inputs))
	for i, in := range args.Inputs {
		if strings.TrimSpace(in.InputDescription) != "" {
			in.InputDescription = NormalizeActionDescription(in.InputDescription)
		} else {
			in.InputDescription = desc
		}
		ins[i] = in
	}
	wire := desktopCreateActionArgsWire{
		Description: desc,
		InputBEEF:   sdkwallet.BytesList(args.InputBEEF),
		Inputs:      ins,
		Outputs:     outs,
		LockTime:    args.LockTime,
		Version:     args.Version,
		Labels:      NormalizeActionLabels(args.Labels...),
		Options:     args.Options,
	}
	data, err := c.call(ctx, "createAction", &wire, originator)
	if err != nil {
		return nil, err
	}
	var raw desktopCreateActionResultWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("desktop wallet createAction decode: %w", err)
	}
	res := &sdkwallet.CreateActionResult{
		Txid:            raw.Txid,
		Tx:              []byte(raw.Tx),
		NoSendChange:    raw.NoSendChange,
		SendWithResults: raw.SendWithResults,
	}
	if raw.SignableTransaction != nil {
		res.SignableTransaction = &sdkwallet.SignableTransaction{
			Tx:        []byte(raw.SignableTransaction.Tx),
			Reference: []byte(raw.SignableTransaction.Reference),
		}
	}
	return res, nil
}

type desktopSignActionResultWire struct {
	Txid            chainhash.Hash             `json:"txid,omitempty"`
	Tx              flexBytes                  `json:"tx,omitempty"`
	SendWithResults []sdkwallet.SendWithResult `json:"sendWithResults,omitempty"`
}

func (c *DesktopJSONClient) SignAction(ctx context.Context, args sdkwallet.SignActionArgs, originator string) (*sdkwallet.SignActionResult, error) {
	data, err := c.call(ctx, "signAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var raw desktopSignActionResultWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("desktop wallet signAction decode: %w", err)
	}
	return &sdkwallet.SignActionResult{
		Txid:            raw.Txid,
		Tx:              []byte(raw.Tx),
		SendWithResults: raw.SendWithResults,
	}, nil
}

func (c *DesktopJSONClient) AbortAction(ctx context.Context, args sdkwallet.AbortActionArgs, originator string) (*sdkwallet.AbortActionResult, error) {
	data, err := c.call(ctx, "abortAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.AbortActionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet abortAction decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) ListActions(ctx context.Context, args sdkwallet.ListActionsArgs, originator string) (*sdkwallet.ListActionsResult, error) {
	data, err := c.call(ctx, "listActions", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ListActionsResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet listActions decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) InternalizeAction(ctx context.Context, args sdkwallet.InternalizeActionArgs, originator string) (*sdkwallet.InternalizeActionResult, error) {
	args.Description = NormalizeActionDescription(args.Description)
	args.Labels = NormalizeActionLabels(args.Labels...)
	data, err := c.call(ctx, "internalizeAction", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.InternalizeActionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet internalizeAction decode: %w", err)
	}
	return &res, nil
}

type desktopListOutputsArgsWire struct {
	Basket                    string                  `json:"basket"`
	Tags                      []string                `json:"tags,omitempty"`
	TagQueryMode              sdkwallet.QueryMode     `json:"tagQueryMode,omitempty"`
	Include                   sdkwallet.OutputInclude `json:"include,omitempty"`
	IncludeCustomInstructions *bool                   `json:"includeCustomInstructions,omitempty"`
	IncludeTags               *bool                   `json:"includeTags,omitempty"`
	IncludeLabels             *bool                   `json:"includeLabels,omitempty"`
	Limit                     *uint32                 `json:"limit,omitempty"`
	Offset                    *uint32                 `json:"offset,omitempty"`
	SeekPermission            *bool                   `json:"seekPermission,omitempty"`
}

func (c *DesktopJSONClient) ListOutputs(ctx context.Context, args sdkwallet.ListOutputsArgs, originator string) (*sdkwallet.ListOutputsResult, error) {
	wire := desktopListOutputsArgsWire{
		Basket:                    args.Basket,
		Tags:                      args.Tags,
		TagQueryMode:              args.TagQueryMode,
		Include:                   args.Include,
		IncludeCustomInstructions: args.IncludeCustomInstructions,
		IncludeTags:               args.IncludeTags,
		IncludeLabels:             args.IncludeLabels,
		Limit:                     args.Limit,
		Offset:                    args.Offset,
		SeekPermission:            args.SeekPermission,
	}
	data, err := c.call(ctx, "listOutputs", &wire, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ListOutputsResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet listOutputs decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) RelinquishOutput(ctx context.Context, args sdkwallet.RelinquishOutputArgs, originator string) (*sdkwallet.RelinquishOutputResult, error) {
	data, err := c.call(ctx, "relinquishOutput", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.RelinquishOutputResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet relinquishOutput decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) RevealCounterpartyKeyLinkage(ctx context.Context, args sdkwallet.RevealCounterpartyKeyLinkageArgs, originator string) (*sdkwallet.RevealCounterpartyKeyLinkageResult, error) {
	data, err := c.call(ctx, "revealCounterpartyKeyLinkage", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.RevealCounterpartyKeyLinkageResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet revealCounterpartyKeyLinkage decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) RevealSpecificKeyLinkage(ctx context.Context, args sdkwallet.RevealSpecificKeyLinkageArgs, originator string) (*sdkwallet.RevealSpecificKeyLinkageResult, error) {
	data, err := c.call(ctx, "revealSpecificKeyLinkage", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.RevealSpecificKeyLinkageResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet revealSpecificKeyLinkage decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) AcquireCertificate(ctx context.Context, args sdkwallet.AcquireCertificateArgs, originator string) (*sdkwallet.Certificate, error) {
	data, err := c.call(ctx, "acquireCertificate", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.Certificate
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet acquireCertificate decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) ListCertificates(ctx context.Context, args sdkwallet.ListCertificatesArgs, originator string) (*sdkwallet.ListCertificatesResult, error) {
	data, err := c.call(ctx, "listCertificates", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ListCertificatesResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet listCertificates decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) ProveCertificate(ctx context.Context, args sdkwallet.ProveCertificateArgs, originator string) (*sdkwallet.ProveCertificateResult, error) {
	data, err := c.call(ctx, "proveCertificate", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.ProveCertificateResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet proveCertificate decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) RelinquishCertificate(ctx context.Context, args sdkwallet.RelinquishCertificateArgs, originator string) (*sdkwallet.RelinquishCertificateResult, error) {
	data, err := c.call(ctx, "relinquishCertificate", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.RelinquishCertificateResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet relinquishCertificate decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) DiscoverByIdentityKey(ctx context.Context, args sdkwallet.DiscoverByIdentityKeyArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	data, err := c.call(ctx, "discoverByIdentityKey", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.DiscoverCertificatesResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet discoverByIdentityKey decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) DiscoverByAttributes(ctx context.Context, args sdkwallet.DiscoverByAttributesArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	data, err := c.call(ctx, "discoverByAttributes", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.DiscoverCertificatesResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet discoverByAttributes decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) IsAuthenticated(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	data, err := c.call(ctx, "isAuthenticated", args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.AuthenticatedResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet isAuthenticated decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) WaitForAuthentication(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	data, err := c.call(ctx, "waitForAuthentication", args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.AuthenticatedResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet waitForAuthentication decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) GetHeight(ctx context.Context, args any, originator string) (*sdkwallet.GetHeightResult, error) {
	data, err := c.call(ctx, "getHeight", args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.GetHeightResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet getHeight decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) GetHeaderForHeight(ctx context.Context, args sdkwallet.GetHeaderArgs, originator string) (*sdkwallet.GetHeaderResult, error) {
	data, err := c.call(ctx, "getHeaderForHeight", &args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.GetHeaderResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet getHeaderForHeight decode: %w", err)
	}
	return &res, nil
}

func (c *DesktopJSONClient) GetNetwork(ctx context.Context, args any, originator string) (*sdkwallet.GetNetworkResult, error) {
	data, err := c.call(ctx, "getNetwork", args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.GetNetworkResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet getNetwork decode: %w", err)
	}
	if res.Network == "" {
		return nil, errors.New("desktop wallet getNetwork returned empty network")
	}
	return &res, nil
}

func (c *DesktopJSONClient) GetVersion(ctx context.Context, args any, originator string) (*sdkwallet.GetVersionResult, error) {
	data, err := c.call(ctx, "getVersion", args, originator)
	if err != nil {
		return nil, err
	}
	var res sdkwallet.GetVersionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("desktop wallet getVersion decode: %w", err)
	}
	return &res, nil
}
