package server

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/jiaweisky/harbor/internal/config"
)

// signCard signs card with the configured key and appends the JWS signature.
func signCard(ctx context.Context, card *a2a.AgentCard, cfg *config.CardSigningConfig) error {
	key, err := loadPrivateKey(cfg.KeyFile)
	if err != nil {
		return err
	}

	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{
		PrivateKey: key,
		KeyID:      cfg.KeyID,
		Algorithm:  cfg.Algorithm,
		JWKSURL:    cfg.JWKSURL,
	})
	if err != nil {
		return fmt.Errorf("card signing: %w", err)
	}

	raw, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card for signing: %w", err)
	}

	sig, err := signer.Sign(ctx, raw)
	if err != nil {
		return fmt.Errorf("sign card: %w", err)
	}

	card.Signatures = append(card.Signatures, *sig)
	return nil
}

// loadPrivateKey reads a PEM private key (PKCS#8, EC, or PKCS#1).
func loadPrivateKey(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("parse signing key %s: no PEM block", path)
	}

	switch block.Type {
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse EC private key: %w", err)
		}
		return key, nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse RSA private key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("private key type %T cannot sign", key)
		}
		return signer, nil
	default:
		return nil, fmt.Errorf("unsupported private key type %q", block.Type)
	}
}
