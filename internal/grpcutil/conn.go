// Package grpcutil provides utilities for gRPC client connections.
package grpcutil

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientConfig contains configuration options for gRPC client connections.
type ClientConfig struct {
	// Timeout is the timeout for connection establishment.
	// A zero value means no timeout.
	Timeout time.Duration

	// MaxRecvMsgSize is the maximum message size receivable from the server.
	// A zero value uses the gRPC default (4MB).
	MaxRecvMsgSize int

	// MaxSendMsgSize is the maximum message size sendable to the server.
	// A zero value uses the gRPC default.
	MaxSendMsgSize int

	// EnableCompression enables gzip compression for the connection.
	EnableCompression bool

	// ExtraOpts allows passing additional gRPC dial options.
	ExtraOpts []grpc.DialOption
}

// DefaultConfig returns a ClientConfig with sensible defaults matching
// current MonoFS usage patterns.
func DefaultConfig() ClientConfig {
	return ClientConfig{
		MaxRecvMsgSize: 256 * 1024 * 1024, // 256MB - matches common usage in client/sharded.go
		MaxSendMsgSize: 256 * 1024 * 1024, // 256MB
	}
}

// NewClient creates a new gRPC client connection with the specified configuration.
// It uses insecure transport credentials by default (suitable for internal cluster
// communication). For secure connections, add appropriate ExtraOpts.
//
// Example usage:
//
//	// Simple connection with defaults
//	conn, err := grpcutil.NewClient("localhost:8080", grpcutil.DefaultConfig())
//
//	// Custom configuration
//	cfg := grpcutil.ClientConfig{
//	    Timeout:        5 * time.Second,
//	    MaxRecvMsgSize: 100 * 1024 * 1024,
//	}
//	conn, err := grpcutil.NewClient("localhost:8080", cfg)
func NewClient(addr string, cfg ClientConfig) (*grpc.ClientConn, error) {
	opts := make([]grpc.DialOption, 0)

	// Always use insecure credentials by default (internal cluster communication)
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	// Apply message size limits if configured
	callOpts := make([]grpc.CallOption, 0)

	if cfg.MaxRecvMsgSize > 0 {
		callOpts = append(callOpts, grpc.MaxCallRecvMsgSize(cfg.MaxRecvMsgSize))
	}

	if cfg.MaxSendMsgSize > 0 {
		callOpts = append(callOpts, grpc.MaxCallSendMsgSize(cfg.MaxSendMsgSize))
	}

	if len(callOpts) > 0 {
		opts = append(opts, grpc.WithDefaultCallOptions(callOpts...))
	}

	// Add compression if enabled
	if cfg.EnableCompression {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")))
	}

	// Add any extra options
	if len(cfg.ExtraOpts) > 0 {
		opts = append(opts, cfg.ExtraOpts...)
	}

	return grpc.NewClient(addr, opts...)
}

// NewSimpleClient creates a new gRPC client connection with default configuration.
// This is a convenience wrapper for the most common use case.
func NewSimpleClient(addr string) (*grpc.ClientConn, error) {
	return NewClient(addr, DefaultConfig())
}

// NewMinimalClient creates a new gRPC client connection without any message size
// limits or additional configuration. Use this when you need the gRPC defaults.
func NewMinimalClient(addr string) (*grpc.ClientConn, error) {
	return NewClient(addr, ClientConfig{})
}
