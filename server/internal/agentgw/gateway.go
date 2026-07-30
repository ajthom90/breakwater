// Package agentgw is the agent-facing gRPC gateway on :9443.
// Append-only is structural: this package must NEVER register destructive RPCs
// (delete, prune, retention mutation). Those live only on the web/REST port.
package agentgw

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// Full method names allowed without prior enrollment (pre-pin).
var enrollMethods = map[string]bool{
	"/breakwater.v1.EnrollmentService/Enroll": true,
	"/breakwater.enroll.Enrollment/Enroll":    true,
}

// Gateway serves the agent gRPC port with mTLS fingerprint pinning.
type Gateway struct {
	Enroll *enroll.Service
	Log    *slog.Logger

	// TestEcho, when non-nil, registers a post-enroll Echo RPC (tests only).
	TestEcho EchoServer

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

	g.gs = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ForceServerCodec(jsonCodec{}),
		grpc.UnaryInterceptor(g.unaryInterceptor),
		grpc.StreamInterceptor(g.streamInterceptor),
	)
	RegisterEnrollmentServer(g.gs, &enrollmentServer{svc: g.Enroll})
	if g.TestEcho != nil {
		RegisterEchoServer(g.gs, g.TestEcho)
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

// --- Hand-written enrollment service (proto-compatible until codegen in CI) ---

// EnrollmentServer is the service interface.
type EnrollmentServer interface {
	Enroll(context.Context, *EnrollRequest) (*EnrollResponse, error)
}

// EnrollRequest mirrors breakwater.v1.EnrollRequest for M1 without codegen.
type EnrollRequest struct {
	Token         string
	Hostname      string
	OS            string
	OSVersion     string
	AgentVersion  string
	Arch          string
	ClientCertPEM []byte
}

// EnrollResponse mirrors breakwater.v1.EnrollResponse.
type EnrollResponse struct {
	MachineID             string
	HashingKey            []byte
	ServerCertFingerprint string
	PolicyID              string
}

type enrollmentServer struct {
	svc *enroll.Service
}

func (e *enrollmentServer) Enroll(ctx context.Context, req *EnrollRequest) (*EnrollResponse, error) {
	pi, ok := PeerFromContext(ctx)
	if !ok || pi.CertFP == "" {
		return nil, status.Error(codes.Unauthenticated, "missing peer certificate")
	}
	resp, err := e.svc.Enroll(ctx, enroll.EnrollRequest{
		Token:            req.Token,
		Hostname:         req.Hostname,
		OS:               req.OS,
		OSVersion:        req.OSVersion,
		AgentVersion:     req.AgentVersion,
		Arch:             req.Arch,
		ClientCertPEM:    req.ClientCertPEM,
		ConnectionCertFP: pi.CertFP, // B2: bind TLS peer, not body alone
	})
	if err != nil {
		// Map known client errors; do not leak raw DB internals (REVIEW-M1 M6 partial).
		return nil, status.Errorf(codes.InvalidArgument, "enroll: %v", err)
	}
	return &EnrollResponse{
		MachineID:             resp.MachineID,
		HashingKey:            resp.HashingKey,
		ServerCertFingerprint: resp.ServerCertFingerprint,
		PolicyID:              resp.PolicyID,
	}, nil
}

// RegisterEnrollmentServer registers the hand-rolled enrollment service.
func RegisterEnrollmentServer(s *grpc.Server, srv EnrollmentServer) {
	s.RegisterService(&Enrollment_ServiceDesc, srv)
}

// Enrollment_ServiceDesc is the gRPC service descriptor for enrollment.
var Enrollment_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "breakwater.enroll.Enrollment",
	HandlerType: (*EnrollmentServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Enroll",
			Handler:    _Enrollment_Enroll_Handler,
		},
	},
	Streams: []grpc.StreamDesc{},
}

func _Enrollment_Enroll_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(EnrollRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EnrollmentServer).Enroll(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/breakwater.enroll.Enrollment/Enroll",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(EnrollmentServer).Enroll(ctx, req.(*EnrollRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// EnrollmentClient is a client for tests / fake agent.
type EnrollmentClient struct {
	cc grpc.ClientConnInterface
}

// NewEnrollmentClient constructs a client.
func NewEnrollmentClient(cc grpc.ClientConnInterface) *EnrollmentClient {
	return &EnrollmentClient{cc: cc}
}

// Enroll calls the enrollment RPC.
func (c *EnrollmentClient) Enroll(ctx context.Context, in *EnrollRequest, opts ...grpc.CallOption) (*EnrollResponse, error) {
	out := new(EnrollResponse)
	opts = append([]grpc.CallOption{grpc.ForceCodec(jsonCodec{})}, opts...)
	err := c.cc.Invoke(ctx, "/breakwater.enroll.Enrollment/Enroll", in, out, opts...)
	return out, err
}

// Echo is a post-enroll test RPC used only in tests to prove pin enforcement.
// Registered only when EnableTestRPCs is called.
type EchoServer interface {
	Echo(context.Context, *EchoRequest) (*EchoResponse, error)
}

// EchoRequest is a test payload.
type EchoRequest struct {
	Message string
}

// EchoResponse is a test payload.
type EchoResponse struct {
	Message   string
	MachineID string
}

// RegisterEchoServer registers the test Echo RPC (not used in production).
func RegisterEchoServer(s *grpc.Server, srv EchoServer) {
	s.RegisterService(&Echo_ServiceDesc, srv)
}

// Echo_ServiceDesc is the test echo service descriptor.
var Echo_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "breakwater.enroll.Echo",
	HandlerType: (*EchoServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Echo",
			Handler:    _Echo_Echo_Handler,
		},
	},
}

func _Echo_Echo_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(EchoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EchoServer).Echo(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/breakwater.enroll.Echo/Echo",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(EchoServer).Echo(ctx, req.(*EchoRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// EchoClient calls the test Echo RPC.
type EchoClient struct {
	cc grpc.ClientConnInterface
}

// NewEchoClient constructs an echo client.
func NewEchoClient(cc grpc.ClientConnInterface) *EchoClient {
	return &EchoClient{cc: cc}
}

// Echo invokes the test method.
func (c *EchoClient) Echo(ctx context.Context, in *EchoRequest, opts ...grpc.CallOption) (*EchoResponse, error) {
	out := new(EchoResponse)
	opts = append([]grpc.CallOption{grpc.ForceCodec(jsonCodec{})}, opts...)
	err := c.cc.Invoke(ctx, "/breakwater.enroll.Echo/Echo", in, out, opts...)
	return out, err
}

// GRPCServer returns the underlying server for test registration.
func (g *Gateway) GRPCServer() *grpc.Server {
	return g.gs
}
