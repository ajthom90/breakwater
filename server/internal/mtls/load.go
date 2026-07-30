package mtls

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// LoadIdentityFromPEM loads a certificate and private key from PEM bytes.
func LoadIdentityFromPEM(certPEM, keyPEM []byte) (*Identity, error) {
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("x509 key pair: %w", err)
	}
	if len(tlsCert.Certificate) == 0 {
		return nil, fmt.Errorf("no certificate in pair")
	}
	cert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("no key PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected ed25519 private key")
	}
	return &Identity{
		PrivateKey: priv,
		Cert:       cert,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TLSCert:    tlsCert,
	}, nil
}
