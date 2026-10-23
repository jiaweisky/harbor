package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

func TestTLSConfigPlaintext(t *testing.T) {
	cfg, err := tlsConfigFor(config.TLSConfig{})
	if err != nil || cfg != nil {
		t.Fatalf("tlsConfigFor(empty) = (%v,%v), want (nil,nil)", cfg, err)
	}
}

func TestTLSConfigMissingKeyPair(t *testing.T) {
	cfg := config.TLSConfig{CertFile: filepath.Join(t.TempDir(), "missing.crt")}
	if _, err := tlsConfigFor(cfg); err == nil {
		t.Fatal("expected an error for a missing certificate")
	}
}

func TestTLSConfigClientAuth(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	got, err := tlsConfigFor(config.TLSConfig{
		CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ClientAuth: true,
	})
	if err != nil {
		t.Fatalf("tlsConfigFor: %v", err)
	}
	if len(got.Certificates) != 1 || got.ClientCAs == nil || got.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("unexpected TLS config: %+v", got)
	}
}

func TestTLSConfigInvalidCA(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tlsConfigFor(config.TLSConfig{CertFile: certFile, KeyFile: keyFile, CAFile: caFile})
	if err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("err = %v, want a no-certificates error", err)
	}
}

// writeTestCert writes a self-signed certificate and key and returns their paths.
func writeTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "harbor-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
