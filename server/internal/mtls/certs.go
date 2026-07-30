// Package mtls provides certificate generation and fingerprint pinning helpers.
// Enrollment: token embeds server cert FP (zero TOFU); agent self-signed cert;
// mutual fingerprint pinning thereafter.
package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CertFingerprintSHA256 returns the SHA-256 hex fingerprint of the DER cert.
func CertFingerprintSHA256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// CertFingerprintFromTLS returns the FP of the leaf certificate.
func CertFingerprintFromTLS(cert *x509.Certificate) string {
	return CertFingerprintSHA256(cert.Raw)
}

// Identity is a keypair + certificate (server or agent).
type Identity struct {
	PrivateKey ed25519.PrivateKey
	Cert       *x509.Certificate
	CertPEM    []byte
	KeyPEM     []byte
	TLSCert    tls.Certificate
}

// Fingerprint returns the SHA-256 cert fingerprint.
func (id *Identity) Fingerprint() string {
	return CertFingerprintSHA256(id.Cert.Raw)
}

// GenerateServerIdentity creates a self-signed server certificate for breakwaterd.
func GenerateServerIdentity(commonName string, hosts []string, validFor time.Duration) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Breakwater"},
			CommonName:   commonName,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(validFor),
		// Leaf server cert for fingerprint pinning — not a CA (REVIEW-M1 M5).
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) == 0 {
		tmpl.DNSNames = []string{commonName, "localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create cert: %w", err)
	}
	return identityFromDER(der, priv)
}

// GenerateAgentIdentity creates a self-signed client certificate for an agent.
func GenerateAgentIdentity(commonName string, validFor time.Duration) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Breakwater Agent"},
			CommonName:   commonName,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create agent cert: %w", err)
	}
	return identityFromDER(der, priv)
}

func identityFromDER(der []byte, priv ed25519.PrivateKey) (*Identity, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := marshalED25519Key(priv)
	if err != nil {
		return nil, err
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &Identity{
		PrivateKey: priv,
		Cert:       cert,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TLSCert:    tlsCert,
	}, nil
}

func marshalED25519Key(priv ed25519.PrivateKey) ([]byte, error) {
	b, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), nil
}

// ParseCertPEM parses the first certificate in PEM data.
func ParseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ServerTLSConfig builds a server tls.Config that requires client certs and
// validates them via the provided VerifyPeerCertificate callback (fingerprint pin).
func ServerTLSConfig(server *Identity, verifyClient func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{server.TLSCert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAnyClientCert,
		// We pin fingerprints ourselves; skip system CA verification.
		VerifyPeerCertificate: verifyClient,
	}
}

// ClientTLSConfig builds a client tls.Config that presents the agent cert and
// pins the server certificate fingerprint (no TOFU).
func ClientTLSConfig(client *Identity, serverFP string) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{client.TLSCert},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by fingerprint pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no server certificate")
			}
			fp := CertFingerprintSHA256(rawCerts[0])
			if fp != serverFP {
				return fmt.Errorf("server certificate fingerprint mismatch: got %s want %s", fp, serverFP)
			}
			return nil
		},
	}
}

// EnrollmentClientTLSConfig is used during Enroll only: pin server FP, no client cert required by server yet
// (client still presents its cert so the server can bind the FP).
func EnrollmentClientTLSConfig(client *Identity, serverFP string) *tls.Config {
	return ClientTLSConfig(client, serverFP)
}
