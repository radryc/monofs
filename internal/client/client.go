// Package client provides a gRPC client wrapper for MonoFS backend communication.
package client

import (
	"context"
	"io"
	"log/slog"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// MonoFSClient is the interface for MonoFS client operations.
// Both simple Client and ShardedClient implement this interface.
type MonoFSClient interface {
	Lookup(ctx context.Context, path string) (*pb.LookupResponse, error)
	GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error)
	ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error)
	Read(ctx context.Context, path string, offset, size int64) ([]byte, error)
	Close() error
	// Metrics tracking
	RecordOperation()
	RecordBytesRead(n int64)
}

// MetricsRecorder provides optional methods for recording operation metrics.
// Clients that support metrics tracking (like ShardedClient) can implement this.
type MetricsRecorder interface {
	RecordOperation()
	RecordBytesRead(n int64)
}

// Client wraps the gRPC client for MonoFS backend.
// Deprecated: Use ShardedClient for distributed operations with HRW sharding.
type Client struct {
	conn   *grpc.ClientConn
	client pb.MonoFSClient
	logger *slog.Logger
}

// Option configures the client.
type Option func(*options)

type options struct {
	creds  credentials.TransportCredentials
	logger *slog.Logger
}

// WithTLSCredentials sets TLS credentials for mTLS (stub for future use).
func WithTLSCredentials(creds credentials.TransportCredentials) Option {
	return func(o *options) {
		o.creds = creds
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		o.logger = logger
	}
}

// New creates a new MonoFS client connected to the specified address.
// Deprecated: Use NewShardedClient for distributed MonoFS deployments.
func New(addr string, opts ...Option) (*Client, error) {
	o := &options{
		creds:  insecure.NewCredentials(), // Default: insecure for initial implementation
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(o)
	}

	logger := o.logger.With("component", "client", "server", addr)

	logger.Info("connecting to backend")
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(o.creds),
	)
	if err != nil {
		logger.Error("failed to connect", "error", err)
		return nil, err
	}

	logger.Info("connected to backend")
	return &Client{
		conn:   conn,
		client: pb.NewMonoFSClient(conn),
		logger: logger,
	}, nil
}

// Lookup finds a child entry in a directory.
func (c *Client) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error) {
	// Split path into parent and name
	parentPath := ""
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		parentPath = path[:idx]
		name = path[idx+1:]
	}

	c.logger.Debug("lookup", "path", path, "parent_path", parentPath, "name", name)
	resp, err := c.client.Lookup(ctx, &pb.LookupRequest{
		ParentPath: parentPath,
		Name:       name,
	})
	if err != nil {
		c.logger.Error("lookup failed", "path", path, "error", err)
	}
	return resp, err
}

// GetAttr retrieves file attributes.
func (c *Client) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	c.logger.Debug("getattr", "path", path)
	resp, err := c.client.GetAttr(ctx, &pb.GetAttrRequest{
		Path: path,
	})
	if err != nil {
		c.logger.Error("getattr failed", "path", path, "error", err)
	}
	return resp, err
}

// ReadDir retrieves directory entries.
func (c *Client) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	c.logger.Debug("readdir rpc call starting", "path", path, "component", "client")
	stream, err := c.client.ReadDir(ctx, &pb.ReadDirRequest{
		Path: path,
	})
	if err != nil {
		c.logger.Error("readdir rpc failed", "path", path, "error", err, "component", "client")
		return nil, err
	}

	c.logger.Debug("readdir rpc stream opened", "path", path, "component", "client")
	var entries []*pb.DirEntry
	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.logger.Error("readdir stream error", "path", path, "error", err, "component", "client")
			return nil, err
		}
		entries = append(entries, entry)
	}
	c.logger.Debug("readdir rpc complete", "path", path, "entries", len(entries), "component", "client")
	return entries, nil
}

// Read retrieves file content.
func (c *Client) Read(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	c.logger.Debug("read", "path", path, "offset", offset, "size", size)
	stream, err := c.client.Read(ctx, &pb.ReadRequest{
		Path:   path,
		Offset: offset,
		Size:   size,
	})
	if err != nil {
		c.logger.Error("read failed", "path", path, "error", err)
		return nil, err
	}

	var data []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.logger.Error("read stream error", "path", path, "error", err)
			return nil, err
		}
		data = append(data, chunk.Data...)
	}
	c.logger.Debug("read complete", "path", path, "bytes", len(data))
	return data, nil
}

// Create creates a new file.
func (c *Client) Create(ctx context.Context, path string, mode uint32) (*pb.CreateResponse, error) {
	c.logger.Debug("create", "path", path, "mode", mode)
	resp, err := c.client.Create(ctx, &pb.CreateRequest{
		ParentPath: path,
		Name:       "",
		Mode:       mode,
		Flags:      0,
	})
	if err != nil {
		c.logger.Error("create failed", "path", path, "error", err)
	}
	return resp, err
}

// Authenticate performs authentication (stub for future mTLS).
func (c *Client) Authenticate(ctx context.Context, token string) (*pb.AuthResponse, error) {
	c.logger.Debug("authenticate")
	return c.client.Authenticate(ctx, &pb.AuthRequest{
		Token: token,
	})
}

// Close closes the client connection.
func (c *Client) Close() error {
	c.logger.Info("closing connection")
	return c.conn.Close()
}

// RecordOperation is a no-op for simple client (metrics only tracked in ShardedClient).
func (c *Client) RecordOperation() {
	// No-op for simple client
}

// RecordBytesRead is a no-op for simple client (metrics only tracked in ShardedClient).
func (c *Client) RecordBytesRead(n int64) {
	// No-op for simple client
}
