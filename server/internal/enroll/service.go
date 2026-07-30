package enroll

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/oklog/ulid/v2"
)

// Typed enrollment errors for gateway mapping (R2-11).
var (
	ErrInvalidToken      = errors.New("invalid enrollment token")
	ErrTokenUsed         = errors.New("enrollment token already used")
	ErrTokenExpired      = errors.New("enrollment token expired")
	ErrCertMismatch      = errors.New("client certificate does not match TLS connection")
	ErrAlreadyEnrolled   = errors.New("client certificate already enrolled")
	ErrMissingConnection = errors.New("missing connection certificate fingerprint")
	ErrServerFPMismatch  = errors.New("token server fingerprint does not match this server")
)

// Keystore stores per-repo secrets. Narrow interface to avoid vault import cycles.
type Keystore interface {
	// CreateRepoPassword generates and stores a repo password (hashing key set later).
	CreateRepoPassword(ctx context.Context, repoID string) (repoPassword string, err error)
	// SetHashingKey stores the vault-sourced content-ID HMAC secret and algorithm.
	SetHashingKey(ctx context.Context, repoID string, hashingKey []byte, algorithm string) error
	// DeleteRepo removes a keystore row (compensation on enroll failure).
	DeleteRepo(ctx context.Context, repoID string) error
}

// VaultCreator initializes a per-machine vault and returns its hashing key.
type VaultCreator interface {
	// Create initializes the repo and returns (hashingSecret, algorithm, error).
	// Hashing secret is from kopia ContentFormat — never a random placeholder.
	Create(ctx context.Context, repoID, password string) (hashingKey []byte, algorithm string, err error)
}

// Service handles enrollment: token consume → machine row → repo → hashing key.
type Service struct {
	DB            *catalog.DB
	Keystore      Keystore
	Vaults        VaultCreator
	ServerFP      string
	DefaultPolicy string
	Log           *slog.Logger
}

// EnrollRequest is the in-process enroll payload (mirrors proto without codegen dependency).
type EnrollRequest struct {
	Token         string
	Hostname      string
	OS            string
	OSVersion     string
	AgentVersion  string
	Arch          string
	ClientCertPEM []byte // optional; if set must match ConnectionCertFP
	// ConnectionCertFP is the SHA-256 fingerprint of the TLS peer certificate.
	// Required. Identity is always bound from the connection, never the body alone.
	ConnectionCertFP string
}

// EnrollResponse is returned to the agent after successful enrollment.
type EnrollResponse struct {
	MachineID             string
	HashingKey            []byte
	HashingAlgorithm      string
	ServerCertFingerprint string
	PolicyID              string
}

// Enroll performs the enrollment handshake.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	tok, err := Parse(req.Token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if tok.ServerFP != s.ServerFP {
		return nil, ErrServerFPMismatch
	}

	// B2: identity comes from the TLS connection peer, not the request body alone.
	if req.ConnectionCertFP == "" {
		return nil, ErrMissingConnection
	}
	clientFP := req.ConnectionCertFP

	if len(req.ClientCertPEM) > 0 {
		cert, err := mtls.ParseCertPEM(req.ClientCertPEM)
		if err != nil {
			return nil, fmt.Errorf("%w: parse body cert: %v", ErrCertMismatch, err)
		}
		bodyFP := mtls.CertFingerprintFromTLS(cert)
		if bodyFP != clientFP {
			return nil, ErrCertMismatch
		}
	}

	// Reject if this cert is already enrolled.
	existing, err := s.DB.MachineByCertFP(ctx, clientFP)
	if err != nil {
		return nil, err // internal
	}
	if existing != nil {
		return nil, fmt.Errorf("%w as %s", ErrAlreadyEnrolled, existing.ID)
	}

	entropy := ulid.Monotonic(rand.Reader, 0)
	machineID := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()

	tokenID, err := s.DB.ConsumeEnrollToken(ctx, tok.Secret, machineID)
	if err != nil {
		return nil, mapTokenError(err)
	}

	// R2-9 / R3-3: compensate on failure — un-consume token and remove keystore row.
	// MUST use a fresh context: the request ctx may already be canceled/deadline-
	// exceeded (the failure class that co-occurs with slow vault create). modernc
	// sqlite returns ctx.Err() before executing when the context is done.
	success := false
	var keystoreCreated bool
	defer func() {
		if success {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.DB.ReleaseEnrollToken(cctx, tokenID); err != nil && s.Log != nil {
			s.Log.Error("enroll compensate: release token", "token_id", tokenID, "err", err)
		}
		if keystoreCreated {
			if err := s.Keystore.DeleteRepo(cctx, machineID); err != nil && s.Log != nil {
				s.Log.Error("enroll compensate: delete keystore", "repo_id", machineID, "err", err)
			}
		}
		// Orphaned on-disk repo under machineID may remain; log for operator cleanup.
		if s.Log != nil {
			s.Log.Warn("enroll failed after token consume; compensated token/keystore; orphaned repo dir may need cleanup",
				"machine_id", machineID)
		}
	}()

	repoPassword, err := s.Keystore.CreateRepoPassword(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w", err)
	}
	keystoreCreated = true

	var hashingKey []byte
	var hashingAlgo string
	if s.Vaults != nil {
		hashingKey, hashingAlgo, err = s.Vaults.Create(ctx, machineID, repoPassword)
		if err != nil {
			return nil, fmt.Errorf("create vault: %w", err)
		}
		if len(hashingKey) == 0 {
			return nil, fmt.Errorf("vault returned empty hashing key")
		}
		if hashingAlgo == "" {
			return nil, fmt.Errorf("vault returned empty hashing algorithm")
		}
		if err := s.Keystore.SetHashingKey(ctx, machineID, hashingKey, hashingAlgo); err != nil {
			return nil, fmt.Errorf("store hashing key: %w", err)
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

	success = true

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
		HashingAlgorithm:      hashingAlgo,
		ServerCertFingerprint: s.ServerFP,
		PolicyID:              policyID,
	}, nil
}

func mapTokenError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case msg == "invalid enrollment token":
		return ErrInvalidToken
	case msg == "enrollment token already used":
		return ErrTokenUsed
	case msg == "enrollment token expired":
		return ErrTokenExpired
	default:
		// Preserve catalog errors that already wrap our sentinels, or wrap generically.
		if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrTokenUsed) || errors.Is(err, ErrTokenExpired) {
			return err
		}
		// catalog returns fmt.Errorf strings — map by substring.
		if contains(msg, "already used") {
			return ErrTokenUsed
		}
		if contains(msg, "expired") {
			return ErrTokenExpired
		}
		if contains(msg, "invalid enrollment") {
			return ErrInvalidToken
		}
		return err
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// KnownClientFingerprint looks up an enrolled machine by client cert FP.
func (s *Service) KnownClientFingerprint(fp string) (machineID string, ok bool) {
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
