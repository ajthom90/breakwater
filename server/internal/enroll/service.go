package enroll

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/oklog/ulid/v2"
)

// Keystore stores per-repo secrets. Narrow interface to avoid vault import cycles.
type Keystore interface {
	// CreateRepoSecrets generates and stores password + hashing key for a repo.
	// Returns (repoPassword, hashingKey, error).
	CreateRepoSecrets(ctx context.Context, repoID string) (repoPassword string, hashingKey []byte, err error)
}

// VaultCreator initializes a per-machine vault repository.
type VaultCreator interface {
	Create(ctx context.Context, repoID, password string) error
}

// Service handles enrollment: token consume → machine row → repo → hashing key.
type Service struct {
	DB           *catalog.DB
	Keystore     Keystore
	Vaults       VaultCreator
	ServerFP     string
	DefaultPolicy string
	Log          *slog.Logger
}

// EnrollRequest is the in-process enroll payload (mirrors proto without codegen dependency).
type EnrollRequest struct {
	Token         string
	Hostname      string
	OS            string
	OSVersion     string
	AgentVersion  string
	Arch          string
	ClientCertPEM []byte
}

// EnrollResponse is returned to the agent after successful enrollment.
type EnrollResponse struct {
	MachineID              string
	HashingKey             []byte
	ServerCertFingerprint  string
	PolicyID               string
}

// Enroll performs the enrollment handshake.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	tok, err := Parse(req.Token)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	if tok.ServerFP != s.ServerFP {
		return nil, fmt.Errorf("token server fingerprint does not match this server")
	}

	cert, err := mtls.ParseCertPEM(req.ClientCertPEM)
	if err != nil {
		return nil, fmt.Errorf("client cert: %w", err)
	}
	clientFP := mtls.CertFingerprintFromTLS(cert)

	// Reject if this cert is already enrolled.
	existing, err := s.DB.MachineByCertFP(ctx, clientFP)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("client certificate already enrolled as %s", existing.ID)
	}

	entropy := ulid.Monotonic(rand.Reader, 0)
	machineID := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()

	tokenID, err := s.DB.ConsumeEnrollToken(ctx, tok.Secret, machineID)
	if err != nil {
		return nil, err
	}
	_ = tokenID

	repoPassword, hashingKey, err := s.Keystore.CreateRepoSecrets(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w", err)
	}
	if s.Vaults != nil {
		if err := s.Vaults.Create(ctx, machineID, repoPassword); err != nil {
			return nil, fmt.Errorf("create vault: %w", err)
		}
	}

	osInfo := fmt.Sprintf("%s/%s %s", req.OS, req.Arch, req.OSVersion)
	m := catalog.Machine{
		ID:           machineID,
		CertFP:       clientFP,
		Hostname:     req.Hostname,
		OSInfo:       osInfo,
		AgentVersion: req.AgentVersion,
		Status:       "enrolled",
		RepoID:       machineID,
	}
	if err := s.DB.InsertMachine(ctx, m); err != nil {
		return nil, fmt.Errorf("insert machine: %w", err)
	}

	if s.Log != nil {
		s.Log.Info("machine enrolled", "machine_id", machineID, "hostname", req.Hostname, "cert_fp", clientFP[:16]+"…")
	}

	policyID := s.DefaultPolicy
	if policyID == "" {
		policyID = "01DEFAULTPOLICY000000000000"
	}
	return &EnrollResponse{
		MachineID:             machineID,
		HashingKey:            hashingKey,
		ServerCertFingerprint: s.ServerFP,
		PolicyID:              policyID,
	}, nil
}

// VerifyClientCert is a tls.Config VerifyPeerCertificate for the agent port.
// During enrollment the client may not yet be in the catalog — Enroll RPC
// is allowed with any valid client cert; other RPCs require a known FP.
//
// allowedUnknown is true when the connection may be an enroll attempt
// (checked by the gRPC interceptor via peer state). For the M1 gateway we use
// a two-phase approach: RequireAnyClientCert + per-RPC pin check.
func (s *Service) KnownClientFingerprint(fp string) (machineID string, ok bool) {
	// Synchronous lookup — catalog is local SQLite.
	m, err := s.DB.MachineByCertFP(context.Background(), fp)
	if err != nil || m == nil {
		return "", false
	}
	if m.Status == "removed" || m.Status == "disabled" {
		return "", false
	}
	return m.ID, true
}

// ClientFPFromTLSConn extracts the first peer cert fingerprint.
func ClientFPFromTLSConn(state tls.ConnectionState) (string, error) {
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("no peer certificate")
	}
	return mtls.CertFingerprintFromTLS(state.PeerCertificates[0]), nil
}
