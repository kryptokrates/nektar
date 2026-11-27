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
	if !isLoopbackTCPReachable(ctx, baseURL, 250*time.Millisecond) {
		return nil, fmt.Errorf("desktop wallet %s unreachable (TCP connect failed)", baseURL)
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
