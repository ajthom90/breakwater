package audit

import (
	"context"

	"google.golang.org/grpc"
)

// UnaryServerInterceptor returns a no-op-by-default unary interceptor.
// Enrollment-specific audit is emitted from the EnrollmentService handler
// (machine.enroll with success/reject outcomes). Pin rejections for non-enroll
// methods are audited in the gateway's auth interceptor.
//
// Scope (M2 stage 2): does NOT audit ControlService or DataService traffic.
// Agent heartbeats / channel messages are out of scope (see package comment).
// Registered so later stages can add human API method-level audit without
// rewiring the gateway chain.
func (w *Writer) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is the stream counterpart (pass-through for now).
// Intentionally does not audit ControlService.Channel — see package audit policy.
func (w *Writer) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, ss)
	}
}
