// Package server implements the MonoFS gRPC server.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

// StubServer implements the MonoFS gRPC server with in-memory stub data.
type StubServer struct {
	pb.UnimplementedMonoFSServer

	nodeID    string
	address   string
	startTime time.Time
	logger    *slog.Logger
	mu        sync.RWMutex

	// In-memory filesystem structure
	files map[string]*fileEntry

	// Stats
	filesServed atomic.Uint64
}

type fileEntry struct {
	isDir   bool
	mode    uint32
	size    uint64
	content []byte
	mtime   int64
	ino     uint64
}

// NewStubServer creates a new stub server with sample data.
func NewStubServer(nodeID, address string, logger *slog.Logger) *StubServer {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "server", "node_id", nodeID)

	s := &StubServer{
		nodeID:    nodeID,
		address:   address,
		startTime: time.Now(),
		logger:    logger,
		files:     make(map[string]*fileEntry),
	}

	// Initialize with node-specific stub data
	s.initStubData()

	return s
}

func (s *StubServer) initStubData() {
	now := time.Now().Unix()

	// Root directory
	s.files[""] = &fileEntry{
		isDir: true,
		mode:  0755 | uint32(syscall.S_IFDIR),
		mtime: now,
		ino:   1,
	}

	// Sample directories
	s.files["src"] = &fileEntry{
		isDir: true,
		mode:  0755 | uint32(syscall.S_IFDIR),
		mtime: now,
		ino:   2,
	}

	s.files["docs"] = &fileEntry{
		isDir: true,
		mode:  0755 | uint32(syscall.S_IFDIR),
		mtime: now,
		ino:   3,
	}

	// Node-specific directory to demonstrate sharding
	nodeDir := fmt.Sprintf("node-%s", s.nodeID)
	s.files[nodeDir] = &fileEntry{
		isDir: true,
		mode:  0755 | uint32(syscall.S_IFDIR),
		mtime: now,
		ino:   4,
	}

	// Sample files with node-specific content
	readme := []byte(fmt.Sprintf(`# MonoFS

A distributed FUSE filesystem for Git with dedicated database caching and Reed-Solomon data protection.

**Served by node: %s**

## Features

- FUSE-based filesystem
- gRPC client-server architecture
- Metadata caching with NutsDB
- Reed-Solomon erasure coding (planned)
- HRW (Rendezvous) sharding

## Usage

Mount the filesystem:
`+"`"+`bash
./monofs-client --router=localhost:9090 --mount=/mnt/monofs
`+"`"+`
`, s.nodeID))
	s.files["README.md"] = &fileEntry{
		isDir:   false,
		mode:    0644 | uint32(syscall.S_IFREG),
		size:    uint64(len(readme)),
		content: readme,
		mtime:   now,
		ino:     10,
	}

	mainGo := []byte(fmt.Sprintf(`package main

import "fmt"

func main() {
	fmt.Println("Hello from MonoFS node %s!")
}
`, s.nodeID))
	s.files["src/main.go"] = &fileEntry{
		isDir:   false,
		mode:    0644 | uint32(syscall.S_IFREG),
		size:    uint64(len(mainGo)),
		content: mainGo,
		mtime:   now,
		ino:     20,
	}

	utilsGo := []byte(`package main

// Helper utilities for MonoFS
func helper() string {
	return "I'm helping!"
}
`)
	s.files["src/utils.go"] = &fileEntry{
		isDir:   false,
		mode:    0644 | uint32(syscall.S_IFREG),
		size:    uint64(len(utilsGo)),
		content: utilsGo,
		mtime:   now,
		ino:     21,
	}

	docsMd := []byte(`# Documentation

This is the documentation for MonoFS.

## Architecture

The system consists of:
1. FUSE client
2. gRPC backend server
3. Git storage layer
4. Cache layer
5. Router for cluster topology
6. HRW sharding for distribution
`)
	s.files["docs/architecture.md"] = &fileEntry{
		isDir:   false,
		mode:    0644 | uint32(syscall.S_IFREG),
		size:    uint64(len(docsMd)),
		content: docsMd,
		mtime:   now,
		ino:     30,
	}

	// Node-specific info file
	nodeInfo := []byte(fmt.Sprintf(`Node ID: %s
Address: %s
Started: %s
`, s.nodeID, s.address, s.startTime.Format(time.RFC3339)))
	s.files[nodeDir+"/info.txt"] = &fileEntry{
		isDir:   false,
		mode:    0644 | uint32(syscall.S_IFREG),
		size:    uint64(len(nodeInfo)),
		content: nodeInfo,
		mtime:   now,
		ino:     40,
	}

	s.logger.Info("stub data initialized", "files", len(s.files))
}

// Register registers the server with a gRPC server.
func (s *StubServer) Register(grpcServer *grpc.Server) {
	pb.RegisterMonoFSServer(grpcServer, s)
}

// Lookup implements the Lookup RPC.
func (s *StubServer) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	path := req.ParentPath
	if path == "" && req.Name != "" {
		path = req.Name
	} else if path != "" && req.Name != "" {
		path = path + "/" + req.Name
	}

	s.logger.Debug("lookup", "path", path)

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.files[path]
	if !ok {
		return &pb.LookupResponse{Found: false}, nil
	}

	return &pb.LookupResponse{
		Ino:   entry.ino,
		Mode:  entry.mode,
		Size:  entry.size,
		Mtime: entry.mtime,
		Found: true,
	}, nil
}

// GetAttr implements the GetAttr RPC.
func (s *StubServer) GetAttr(ctx context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	path := req.Path
	s.logger.Debug("getattr", "path", path)

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.files[path]
	if !ok {
		return &pb.GetAttrResponse{Found: false}, nil
	}

	nlink := uint32(1)
	if entry.isDir {
		nlink = 2
	}

	return &pb.GetAttrResponse{
		Ino:   entry.ino,
		Mode:  entry.mode,
		Size:  entry.size,
		Mtime: entry.mtime,
		Atime: entry.mtime,
		Ctime: entry.mtime,
		Nlink: nlink,
		Uid:   uint32(1000),
		Gid:   uint32(1000),
		Found: true,
	}, nil
}

// ReadDir implements the ReadDir RPC (streaming).
func (s *StubServer) ReadDir(req *pb.ReadDirRequest, stream grpc.ServerStreamingServer[pb.DirEntry]) error {
	path := req.Path
	s.logger.Debug("readdir", "path", path)

	s.mu.RLock()
	defer s.mu.RUnlock()

	prefix := path
	if prefix != "" {
		prefix = prefix + "/"
	}

	for filePath, entry := range s.files {
		// Skip root itself
		if filePath == path {
			continue
		}

		// Check if this is a direct child
		if !strings.HasPrefix(filePath, prefix) && path != "" {
			continue
		}

		// Get relative path
		relPath := filePath
		if prefix != "" {
			relPath = strings.TrimPrefix(filePath, prefix)
		}

		// Skip if this is a deeper nested entry
		if strings.Contains(relPath, "/") {
			continue
		}

		// Skip empty names
		if relPath == "" {
			continue
		}

		if err := stream.Send(&pb.DirEntry{
			Name: relPath,
			Mode: entry.mode,
			Ino:  entry.ino,
		}); err != nil {
			return err
		}
	}

	return nil
}

// Read implements the Read RPC (streaming).
func (s *StubServer) Read(req *pb.ReadRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	path := req.Path
	offset := req.Offset
	size := req.Size

	s.logger.Debug("read", "path", path, "offset", offset, "size", size)

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.files[path]
	if !ok {
		return io.EOF
	}

	if entry.isDir {
		return io.EOF
	}

	content := entry.content

	// Handle offset
	if offset >= int64(len(content)) {
		return nil
	}
	content = content[offset:]

	// Handle size (0 means read all)
	if size > 0 && size < int64(len(content)) {
		content = content[:size]
	}

	// Stream in chunks of 64KB
	chunkSize := 64 * 1024
	currentOffset := offset

	for len(content) > 0 {
		chunk := content
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}

		if err := stream.Send(&pb.DataChunk{
			Data:   chunk,
			Offset: currentOffset,
		}); err != nil {
			return err
		}

		content = content[len(chunk):]
		currentOffset += int64(len(chunk))
	}

	return nil
}

// Create implements the Create RPC.
func (s *StubServer) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	path := req.ParentPath
	if req.Name != "" {
		if path != "" {
			path = path + "/" + req.Name
		} else {
			path = req.Name
		}
	}

	s.logger.Debug("create", "path", path, "mode", req.Mode)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate a simple inode number
	ino := uint64(len(s.files) + 100)

	s.files[path] = &fileEntry{
		isDir:   false,
		mode:    req.Mode | uint32(syscall.S_IFREG),
		size:    0,
		content: []byte{},
		mtime:   time.Now().Unix(),
		ino:     ino,
	}

	return &pb.CreateResponse{
		Ino:     ino,
		Fh:      ino, // Use ino as file handle for simplicity
		Flags:   0,
		Success: true,
	}, nil
}

// Write implements the Write RPC (client streaming).
func (s *StubServer) Write(stream grpc.ClientStreamingServer[pb.WriteRequest, pb.WriteResponse]) error {
	var totalWritten uint32

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.WriteResponse{
				Size: totalWritten,
			})
		}
		if err != nil {
			return err
		}

		s.logger.Debug("write", "fh", req.Fh, "offset", req.Offset, "len", len(req.Data))

		// For stub, we just count bytes written
		totalWritten += uint32(len(req.Data))
	}
}

// Authenticate implements the Authenticate RPC.
func (s *StubServer) Authenticate(ctx context.Context, req *pb.AuthRequest) (*pb.AuthResponse, error) {
	s.logger.Debug("authenticate", "token_len", len(req.Token))

	// Stub: accept any token
	return &pb.AuthResponse{
		Success:   true,
		SessionId: "stub-session-001",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}, nil
}

// GetNodeInfo implements the GetNodeInfo RPC.
func (s *StubServer) GetNodeInfo(ctx context.Context, req *pb.NodeInfoRequest) (*pb.NodeInfoResponse, error) {
	s.logger.Debug("getNodeInfo")

	return &pb.NodeInfoResponse{
		NodeId:        s.nodeID,
		Address:       s.address,
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		FilesServed:   s.filesServed.Load(),
	}, nil
}

// NodeID returns the server's node ID.
func (s *StubServer) NodeID() string {
	return s.nodeID
}
