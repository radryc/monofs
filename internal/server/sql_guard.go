package server

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// requireInternalSQL confines the node-local SQL RPCs to the router. The
// generated pushdown and the caller-supplied partition scope are only
// trustworthy when they come from the control plane; a direct caller could
// otherwise assert an unrestricted scope and read every partition.
func (s *Server) requireInternalSQL(ctx context.Context) error {
	if s == nil || s.internalToken == "" {
		return status.Error(codes.FailedPrecondition,
			"telemetry SQL is disabled: configure --internal-token or MONOFS_TOKEN on the node")
	}
	if isInternalCall(ctx) {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if md == nil {
		return status.Error(codes.PermissionDenied, "telemetry SQL is internal-only")
	}
	for _, value := range md.Get("x-monofs-internal") {
		if subtle.ConstantTimeCompare([]byte(value), []byte(s.internalToken)) == 1 {
			return nil
		}
	}
	for _, value := range md.Get("authorization") {
		token := strings.TrimSpace(value)
		if strings.HasPrefix(strings.ToLower(token), "bearer ") {
			token = strings.TrimSpace(token[len("Bearer "):])
		}
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.internalToken)) == 1 {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "telemetry SQL is internal-only")
}

// SetInternalToken configures the cluster-internal token required by the
// node-local SQL RPCs. An empty value disables those RPCs.
func (s *Server) SetInternalToken(token string) { s.internalToken = strings.TrimSpace(token) }
