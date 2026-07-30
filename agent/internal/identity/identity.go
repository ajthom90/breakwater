// Package identity generates and loads agent mTLS credentials.
// Self-signed ed25519 client certs; server fingerprint pin (zero TOFU).
package identity

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
	"os"
	"path/filepath"
	"time"
)

// Identity is an agent keypair + certificate.
type Identity struct {
	PrivateKey ed25519.PrivateKey
	Cert       *x509.Certificate
	CertPEM    []byte
	KeyPEM     []byte
	TLSCert    tls.Certificate
}

// Fingerprint returns the SHA-256 hex fingerprint of the leaf cert.
func (id *Identity) Fingerprint() string {
	sum := sha256.Sum256(id.Cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Generate creates a self-signed client certificate for enrollment.
func Generate(commonName string, validFor time.Duration) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	if validFor <= 0 {
		validFor = 10 * 365 * 24 * time.Hour
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
	return fromDER(der, priv)
}

func fromDER(der []byte, priv ed25519.PrivateKey) (*Identity, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
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

// Load reads cert.pem + key.pem from dir.
func Load(dir string) (*Identity, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("load identity: no certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	privBlock, _ := pem.Decode(keyPEM)
	if privBlock == nil {
		return nil, fmt.Errorf("load identity: no private key PEM")
	}
	privAny, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := privAny.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("load identity: expected ed25519 private key")
	}
	return &Identity{
		PrivateKey: priv,
		Cert:       cert,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TLSCert:    tlsCert,
	}, nil
}

// Save writes cert.pem and key.pem atomically (temp-then-rename).
// A half-written identity must never be loadable.
func Save(dir string, id *Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "cert.pem"), id.CertPEM, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "key.pem"), id.KeyPEM, 0o600); err != nil {
		// Best-effort: remove cert so a partial pair is not loadable.
		_ = os.Remove(filepath.Join(dir, "cert.pem"))
		return err
	}
	return nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// ClientTLSConfig presents the agent cert and pins the server certificate fingerprint.
func ClientTLSConfig(client *Identity, serverFP string) *tls.Config {
	want := normalizeFP(serverFP)
	return &tls.Config{
		Certificates:       []tls.Certificate{client.TLSCert},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by fingerprint pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no server certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("server certificate fingerprint mismatch: got %s want %s", got, want)
			}
			return nil
		},
	}
}

func normalizeFP(fp string) string {
	out := make([]byte, 0, len(fp))
	for i := 0; i < len(fp); i++ {
		c := fp[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			out = append(out, c)
		}
	}
	return string(out)
}
