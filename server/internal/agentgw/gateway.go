// Package agentgw is the agent-facing gRPC gateway on :9443.
// Append-only is structural: this package must NEVER register destructive RPCs
// (delete, prune, retention mutation). Those live only on the web/REST port.
package agentgw

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// Full method names allowed without prior enrollment (pre-pin).
var enrollMethods = map[string]bool{
	breakwaterv1.EnrollmentService_Enroll_FullMethodName: true,
}

// Gateway serves the agent gRPC port with mTLS fingerprint pinning.
type Gateway struct {
	Enroll *enroll.Service
	Log    *slog.Logger
	// Auditor, when non-nil, receives hash-chained audit events from interceptors
	// and the enrollment handler (machine.enroll and interceptor denials).
	Auditor *audit.Writer

	// TestDataService, when non-nil, registers DataService for post-enroll pin
	// tests only. Production main never sets this.
	TestDataService breakwaterv1.DataServiceServer

	mu       sync.RWMutex
	serverID *mtls.Identity
	gs       *grpc.Server
	lis      net.Listener
}

// New creates a gateway. serverID is the TLS identity for :9443.
func New(serverID *mtls.Identity, enrollSvc *enroll.Service, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		Enroll:   enrollSvc,
		Log:      log,
		serverID: serverID,
	}
}

// ServerFingerprint returns the pinned server cert FP.
func (g *Gateway) ServerFingerprint() string {
	return g.serverID.Fingerprint()
}

// Start begins listening on addr (e.g. "127.0.0.1:0" or ":9443") and serves in a goroutine.
// Returns the actual bound address.
func (g *Gateway) Start(addr string) (string, error) {
	tlsCfg := mtls.ServerTLSConfig(g.serverID, func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("client certificate required")
		}
		return nil
	})

	unary := []grpc.UnaryServerInterceptor{g.unaryInterceptor}
	stream := []grpc.StreamServerInterceptor{g.streamInterceptor}
	if g.Auditor != nil {
		unary = append(unary, g.Auditor.UnaryServerInterceptor())
		stream = append(stream, g.Auditor.StreamServerInterceptor())
	}

	g.gs = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	)
	breakwaterv1.RegisterEnrollmentServiceServer(g.gs, &enrollmentServer{gw: g, svc: g.Enroll})
	if g.TestDataService != nil {
		breakwaterv1.RegisterDataServiceServer(g.gs, g.TestDataService)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	g.lis = lis
	bound := lis.Addr().String()
	g.Log.Info("agent gateway listening", "addr", bound, "server_fp", g.ServerFingerprint()[:16]+"…")
	go func() {
		if err := g.gs.Serve(lis); err != nil {
			g.Log.Error("agent gateway serve ended", "err", err)
		}
	}()
	return bound, nil
}

// GracefulStop stops the server.
func (g *Gateway) GracefulStop() {
	if g.gs != nil {
		g.gs.GracefulStop()
	}
}

// PeerInfo is stashed in context after interceptors extract the cert FP.
type PeerInfo struct {
	CertFP    string
	MachineID string // empty if not yet enrolled
}

type ctxKey int

const peerKey ctxKey = 1

// PeerFromContext returns agent peer info if present.
func PeerFromContext(ctx context.Context) (PeerInfo, bool) {
	v, ok := ctx.Value(peerKey).(PeerInfo)
	return v, ok
}

func (g *Gateway) extractPeer(ctx context.Context) (PeerInfo, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return PeerInfo{}, status.Error(codes.Unauthenticated, "no peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return PeerInfo{}, status.Error(codes.Unauthenticated, "no tls auth info")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return PeerInfo{}, status.Error(codes.Unauthenticated, "no client certificate")
	}
	fp := mtls.CertFingerprintFromTLS(tlsInfo.State.PeerCertificates[0])
	info := PeerInfo{CertFP: fp}
	if mid, ok := g.Enroll.KnownClientFingerprint(fp); ok {
		info.MachineID = mid
	}
	return info, nil
}

func (g *Gateway) unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	pi, err := g.extractPeer(ctx)
	if err != nil {
		return nil, err
	}
	if !enrollMethods[info.FullMethod] && pi.MachineID == "" {
		g.Log.Warn("rejected unknown client cert", "method", info.FullMethod, "fp", pi.CertFP[:16]+"…")
		// Audit pin rejection (security boundary for non-enroll RPCs).
		// WithoutCancel: client cancel must not drop the auth.fail row (S1-F1).
		if g.Auditor != nil {
			if aerr := g.Auditor.Append(context.WithoutCancel(ctx), audit.Event{
				Actor:     pi.CertFP,
				ActorType: audit.ActorAgent,
				Action:    audit.ActionAuthFail,
				Target:    info.FullMethod,
				Detail: map[string]any{
					"outcome": "rejected",
					"reason":  "unknown_client_certificate",
					"method":  info.FullMethod,
				},
			}); aerr != nil {
				g.Log.Error("audit append failed", "action", audit.ActionAuthFail, "actor", pi.CertFP[:16]+"…", "err", aerr)
			}
		}
		return nil, status.Error(codes.PermissionDenied, "unknown client certificate fingerprint")
	}
	ctx = context.WithValue(ctx, peerKey, pi)
	return handler(ctx, req)
}

func (g *Gateway) streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	pi, err := g.extractPeer(ss.Context())
	if err != nil {
		return err
	}
	if !enrollMethods[info.FullMethod] && pi.MachineID == "" {
		// WithoutCancel: client cancel must not drop the auth.fail row (S1-F1).
		if g.Auditor != nil {
			if aerr := g.Auditor.Append(context.WithoutCancel(ss.Context()), audit.Event{
				Actor:     pi.CertFP,
				ActorType: audit.ActorAgent,
				Action:    audit.ActionAuthFail,
				Target:    info.FullMethod,
				Detail: map[string]any{
					"outcome": "rejected",
					"reason":  "unknown_client_certificate",
					"method":  info.FullMethod,
				},
			}); aerr != nil {
				g.Log.Error("audit append failed", "action", audit.ActionAuthFail, "actor", pi.CertFP[:16]+"…", "err", aerr)
			}
		}
		return status.Error(codes.PermissionDenied, "unknown client certificate fingerprint")
	}
	wrapped := &ctxStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), peerKey, pi)}
	return handler(srv, wrapped)
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

// enrollmentServer implements breakwater.v1.EnrollmentService.
type enrollmentServer struct {
	breakwaterv1.UnimplementedEnrollmentServiceServer
	gw  *Gateway
	svc *enroll.Service
}

func (e *enrollmentServer) Enroll(ctx context.Context, req *breakwaterv1.EnrollRequest) (*breakwaterv1.EnrollResponse, error) {
	pi, ok := PeerFromContext(ctx)
	if !ok || pi.CertFP == "" {
		return nil, status.Error(codes.Unauthenticated, "missing peer certificate")
	}

	var hostname, osName, osVersion, agentVersion, arch string
	if ai := req.GetAgentInfo(); ai != nil {
		hostname = ai.GetHostname()
		osName = ai.GetOs()
		osVersion = ai.GetOsVersion()
		agentVersion = ai.GetAgentVersion()
		arch = ai.GetArch()
	}

	resp, err := e.svc.Enroll(ctx, enroll.EnrollRequest{
		Token:            req.GetToken(),
		Hostname:         hostname,
		OS:               osName,
		OSVersion:        osVersion,
		AgentVersion:     agentVersion,
		Arch:             arch,
		ClientCertPEM:    req.GetClientCertPem(),
		ConnectionCertFP: pi.CertFP, // B2: bind TLS peer, not body alone
	})

	// Audit success and rejected enrollment attempts (security boundary).
	e.auditEnroll(ctx, pi.CertFP, hostname, osName, resp, err)

	if err != nil {
		return nil, e.mapEnrollError(err)
	}
	return &breakwaterv1.EnrollResponse{
		MachineId:             resp.MachineID,
		HashingKey:            resp.HashingKey,
		HashingAlgorithm:      resp.HashingAlgorithm,
		ServerCertFingerprint: resp.ServerCertFingerprint,
		PolicyId:              resp.PolicyID,
	}, nil
}

func (e *enrollmentServer) auditEnroll(ctx context.Context, certFP, hostname, osName string, resp *enroll.EnrollResponse, err error) {
	if e.gw == nil || e.gw.Auditor == nil {
		return
	}
	detail := map[string]any{
		"hostname": hostname,
		"os":       osName,
	}
	target := ""
	if err != nil {
		detail["outcome"] = "rejected"
		detail["reason"] = err.Error()
	} else {
		detail["outcome"] = "success"
		if resp != nil {
			target = resp.MachineID
		}
	}
	// WithoutCancel: client cancel/deadline must not drop machine.enroll (S1-F1).
	// Always log append failures (never silent discard).
	if aerr := e.gw.Auditor.Append(context.WithoutCancel(ctx), audit.Event{
		Actor:     certFP,
		ActorType: audit.ActorAgent,
		Action:    audit.ActionMachineEnroll,
		Target:    target,
		Detail:    detail,
	}); aerr != nil {
		log := e.gw.Log
		if log == nil && e.svc != nil {
			log = e.svc.Log
		}
		if log != nil {
			log.Error("audit append failed", "action", audit.ActionMachineEnroll, "actor", certFP[:min(16, len(certFP))]+"…", "err", aerr)
		}
	}
}

// mapEnrollError maps typed enroll errors to gRPC codes (R2-11).
// Client-fault cases → InvalidArgument/PermissionDenied with safe messages.
// Everything else → Internal with a generic message; details stay in server logs.
func (e *enrollmentServer) mapEnrollError(err error) error {
	switch {
	case errors.Is(err, enroll.ErrInvalidToken),
		errors.Is(err, enroll.ErrTokenExpired),
		errors.Is(err, enroll.ErrMissingConnection),
		errors.Is(err, enroll.ErrServerFPMismatch),
		errors.Is(err, enroll.ErrCertMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, enroll.ErrTokenUsed),
		errors.Is(err, enroll.ErrAlreadyEnrolled):
		return status.Error(codes.PermissionDenied, err.Error())
	default:
		// Log-worthy internal path — do not echo DB/vault paths to the client.
		if e.svc != nil && e.svc.Log != nil {
			e.svc.Log.Error("enrollment internal failure", "err", err)
		}
		return status.Error(codes.Internal, "enrollment failed")
	}
}

// GRPCServer returns the underlying server for test registration.
func (g *Gateway) GRPCServer() *grpc.Server {
	return g.gs
}

// PostEnrollProbe is a minimal DataService used only in tests to prove that
// fingerprint pinning admits enrolled certs and rejects unknown ones.
type PostEnrollProbe struct {
	breakwaterv1.UnimplementedDataServiceServer
}

// CheckContents succeeds for any enrolled peer (interceptor already gated).
// Clients observe success vs PermissionDenied; machine_id is verified via catalog.
func (p *PostEnrollProbe) CheckContents(ctx context.Context, _ *breakwaterv1.CheckContentsRequest) (*breakwaterv1.CheckContentsResponse, error) {
	// Peer is required; empty MachineID should have been rejected by the interceptor.
	if pi, ok := PeerFromContext(ctx); !ok || pi.MachineID == "" {
		return nil, status.Error(codes.PermissionDenied, "not enrolled")
	}
	return &breakwaterv1.CheckContentsResponse{}, nil
}
