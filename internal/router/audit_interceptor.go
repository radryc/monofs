package router

import (
	"context"
	"strings"

	"github.com/radryc/monofs/pkg/audit"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// AuditUnaryInterceptor emits one durable audit event for every mutating
// control-plane RPC. Reads are audited by the storage-node AccessControl, so
// this interceptor deliberately ignores them to avoid duplicate rows.
func AuditUnaryInterceptor(emitter audit.Emitter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		action, auditable := auditActionForMethod(info.FullMethod)
		if !auditable || emitter == nil {
			return handler(ctx, req)
		}
		resp, err := handler(ctx, req)
		if auditErr := emitRouterAudit(ctx, emitter, info.FullMethod, action, req, err); auditErr != nil {
			return nil, status.Error(codes.Unavailable, "audit_unavailable")
		}
		return resp, err
	}
}

// AuditStreamInterceptor is the streaming counterpart of AuditUnaryInterceptor.
func AuditStreamInterceptor(emitter audit.Emitter) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		action, auditable := auditActionForMethod(info.FullMethod)
		if !auditable || emitter == nil {
			return handler(srv, ss)
		}
		err := handler(srv, ss)
		if auditErr := emitRouterAudit(ss.Context(), emitter, info.FullMethod, action, nil, err); auditErr != nil {
			return status.Error(codes.Unavailable, "audit_unavailable")
		}
		return err
	}
}

func auditActionForMethod(fullMethod string) (audit.Action, bool) {
	name := fullMethod
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 {
		name = fullMethod[idx+1:]
	}
	switch {
	case strings.HasPrefix(name, "Delete"), strings.HasPrefix(name, "Remove"):
		return audit.ActionDelete, true
	case strings.HasPrefix(name, "Drain"), strings.HasPrefix(name, "RequestFailover"),
		strings.HasPrefix(name, "Unregister"), strings.HasPrefix(name, "Whitelist"),
		strings.HasPrefix(name, "Rebalance"), strings.HasPrefix(name, "Promote"):
		return audit.ActionAdmin, true
	case strings.HasPrefix(name, "Upsert"), strings.HasPrefix(name, "Ingest"),
		strings.HasPrefix(name, "Publish"), strings.HasPrefix(name, "Create"),
		strings.HasPrefix(name, "Write"), strings.HasPrefix(name, "Update"),
		strings.HasPrefix(name, "Put"), strings.HasPrefix(name, "Commit"),
		strings.HasPrefix(name, "Push"):
		return audit.ActionWrite, true
	}
	return "", false
}

func emitRouterAudit(ctx context.Context, emitter audit.Emitter, method string, action audit.Action, req any, handlerErr error) error {
	decision := audit.DecisionAllow
	reason := ""
	if handlerErr != nil {
		decision = audit.DecisionDeny
		reason = status.Code(handlerErr).String()
	}
	identity, _ := authz.IdentityFromContext(ctx)
	resource := method
	if path := auditRequestPath(req); path != "" {
		resource = method + " " + path
	}
	ev := audit.Event{
		Service:       "monofs-router",
		Scope:         "control-plane",
		Action:        action,
		Decision:      decision,
		Reason:        reason,
		ActorSubject:  identity.Subject,
		ActorClientID: identity.ClientID,
		Email:         identity.Email,
		Groups:        identity.Groups,
		Resource:      resource,
		ResourceType:  "rpc",
		Method:        method,
		RequestID:     requestIDFromGRPC(ctx),
		SourceIP:      remoteAddrFromGRPC(ctx),
		PolicyVersion: "",
	}
	return emitter.EmitDurable(ctx, ev)
}

func auditRequestPath(req any) string {
	if req == nil {
		return ""
	}
	if v, ok := req.(interface{ GetDisplayPath() string }); ok {
		return v.GetDisplayPath()
	}
	if v, ok := req.(interface{ GetLogicalPath() string }); ok {
		return v.GetLogicalPath()
	}
	if v, ok := req.(interface{ GetStorageId() string }); ok {
		return v.GetStorageId()
	}
	return ""
}

func requestIDFromGRPC(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, key := range []string{"x-request-id", "x-correlation-id"} {
		if values := md.Get(key); len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func remoteAddrFromGRPC(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
