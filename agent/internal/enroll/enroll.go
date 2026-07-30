// Package enroll implements the agent-side enrollment client.
//
// Flow: parse BW1 token → generate ed25519 keypair + self-signed cert →
// pin server FP from token (zero TOFU) → EnrollmentService.Enroll over mTLS →
// persist response atomically.
package enroll

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/ajthom90/breakwater/agent/internal/identity"
	"github.com/ajthom90/breakwater/agent/internal/state"
	"github.com/ajthom90/breakwater/agent/internal/token"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
)

// Version is injected by main when available.
var Version = "0.0.1-dev"

// Options configure enrollment.
type Options struct {
	Token    string // raw BW1:... token
	StateDir *state.Dir
	Hostname string // defaults to os.Hostname
	Version  string
	Timeout  time.Duration
}

// Result is the enrolled machine identity.
type Result struct {
	MachineID string
	Meta      *state.Identity
	Creds     *identity.Identity
}

// Run performs enrollment and persists the result under StateDir.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Token == "" || opts.StateDir == nil {
		return nil, fmt.Errorf("enroll: Token and StateDir required")
	}
	if opts.StateDir.IsEnrolled() {
		return nil, fmt.Errorf("enroll: already enrolled (identity present in %s)", opts.StateDir.Path)
	}
	tok, err := token.Parse(opts.Token)
	if err != nil {
		return nil, fmt.Errorf("enroll: parse token: %w", err)
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
		if hostname == "" {
			hostname = "breakwater-agent"
		}
	}
	ver := opts.Version
	if ver == "" {
		ver = Version
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	creds, err := identity.Generate(hostname, 10*365*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("enroll: generate identity: %w", err)
	}

	tlsCfg := identity.ClientTLSConfig(creds, tok.ServerFP)
	conn, err := grpc.NewClient(tok.HostPort,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
	if err != nil {
		return nil, fmt.Errorf("enroll: dial %s: %w", tok.HostPort, err)
	}
	defer conn.Close()

	resp, err := breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: tok.Raw,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname:     hostname,
			Os:           runtime.GOOS,
			OsVersion:    runtime.GOOS + "/" + runtime.GOARCH,
			AgentVersion: ver,
			Arch:         runtime.GOARCH,
		},
		ClientCertPem: creds.CertPEM,
	})
	if err != nil {
		return nil, fmt.Errorf("enroll RPC: %w", err)
	}
	if resp.GetMachineId() == "" || len(resp.GetHashingKey()) == 0 || resp.GetHashingAlgorithm() == "" {
		return nil, fmt.Errorf("enroll: incomplete response")
	}
	// Confirm server FP matches token (defense in depth).
	if fp := resp.GetServerCertFingerprint(); fp != "" && fp != tok.ServerFP {
		return nil, fmt.Errorf("enroll: server FP mismatch in response")
	}

	meta := &state.Identity{
		MachineID:        resp.GetMachineId(),
		ServerAddr:       tok.HostPort,
		ServerFP:         tok.ServerFP,
		HashingAlgorithm: resp.GetHashingAlgorithm(),
		HashingKeyB64:    base64.StdEncoding.EncodeToString(resp.GetHashingKey()),
		Hostname:         hostname,
	}
	if err := opts.StateDir.SaveEnrolled(meta, creds); err != nil {
		return nil, fmt.Errorf("enroll: persist: %w", err)
	}
	return &Result{MachineID: meta.MachineID, Meta: meta, Creds: creds}, nil
}
