// Package agent is the public API surface for the Breakwater Windows agent.
//
// Implementation lives under internal/; this package re-exports the control
// loop, enrollment, and state types so server integration tests (and later
// tooling) can drive a real agent without importing internal paths.
package agent

import (
	"context"
	"crypto/tls"

	"github.com/ajthom90/breakwater/agent/internal/control"
	"github.com/ajthom90/breakwater/agent/internal/enroll"
	"github.com/ajthom90/breakwater/agent/internal/identity"
	"github.com/ajthom90/breakwater/agent/internal/state"
)

// Re-exported types.
type (
	StateDir      = state.Dir
	StateIdentity = state.Identity
	Creds         = identity.Identity
	ControlAgent  = control.Agent
	ControlConfig = control.Config
	EnrollOptions = enroll.Options
	EnrollResult  = enroll.Result
)

// Defaults.
const (
	DefaultStateDir  = state.DefaultStateDir
	MaxCompletedJobs = state.MaxCompletedJobs
)

// ClientParameters is the gRPC keepalive contract (Time=30s).
var ClientParameters = control.ClientParameters

// OpenState opens (and secures) the agent state directory.
func OpenState(path string) (*StateDir, error) {
	return state.Open(path)
}

// Enroll performs token enrollment and persists identity under opts.StateDir.
func Enroll(ctx context.Context, opts EnrollOptions) (*EnrollResult, error) {
	return enroll.Run(ctx, opts)
}

// NewControl creates a control-plane agent. Call (*ControlAgent).Run.
func NewControl(cfg ControlConfig) *ControlAgent {
	return control.New(cfg)
}

// ClientTLSConfig pins the server cert fingerprint and presents the agent cert.
func ClientTLSConfig(client *Creds, serverFP string) *tls.Config {
	return identity.ClientTLSConfig(client, serverFP)
}
