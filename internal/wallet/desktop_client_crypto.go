package wallet

import (
	"context"
	"encoding/json"
	"fmt"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
)

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
