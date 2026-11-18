package router

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// internalDialOptions returns dial options that present the cluster-internal
// token on router -> node calls. Storage nodes treat it as a policy bypass so
// control-plane reads (file viewer, failover sync, rebalance) keep working on
// clusters with read enforcement. Nil when no token is configured.
//
// Callers frequently hold r.mu's write lock here (health checks and ingestion
// dial nodes while mutating topology), so this must NOT lock r.mu.
func (r *Router) internalDialOptions() []grpc.DialOption {
	token, _ := r.internalTokenValue.Load().(string)
	if token == "" {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(internalUnaryClientInterceptor(token)),
		grpc.WithChainStreamInterceptor(internalStreamClientInterceptor(token)),
	}
}

// withSearchAuthorization attaches the router bearer credential to a
// router -> search RPC unless the context already carries an authorization
// header (a forwarded caller token, for example). It never adds an insecure
// fallback: when no token is configured the context is returned unchanged and
// an enforcing search service rejects the call.
func (r *Router) withSearchAuthorization(ctx context.Context) context.Context {
	token, _ := r.searchTokenValue.Load().(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return ctx
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for _, value := range md.Get("authorization") {
			if strings.TrimSpace(value) != "" {
				return ctx
			}
		}
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// searchDialOptions attaches the router bearer credential to every RPC on a
// search connection. Nil when no token is configured.
func (r *Router) searchDialOptions() []grpc.DialOption {
	token, _ := r.searchTokenValue.Load().(string)
	if strings.TrimSpace(token) == "" {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(r.withSearchAuthorization(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(r.withSearchAuthorization(ctx), desc, cc, method, opts...)
		}),
	}
}

func internalUnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-monofs-internal", token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func internalStreamClientInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-monofs-internal", token)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
