package client

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// captureServer intercepts Lookup calls to verify parameters
type captureServer struct {
	pb.UnimplementedMonoFSServer
	lastParentPath string
	lastName       string
	called         bool
}

func (s *captureServer) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	s.called = true
	s.lastParentPath = req.ParentPath
	s.lastName = req.Name

	return &pb.LookupResponse{
		Found: true,
		Ino:   1,
		Mode:  0755,
	}, nil
}

func setupTestServer(t *testing.T) (*captureServer, *Client, func()) {
	listener := bufconn.Listen(1024 * 1024)

	capture := &captureServer{}

	server := grpc.NewServer()
	pb.RegisterMonoFSServer(server, capture)

	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("Server error: %v", err)
		}
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}

	conn, err := grpc.DialContext(
		context.Background(),
		"bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	client := &Client{
		client: pb.NewMonoFSClient(conn),
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	cleanup := func() {
		conn.Close()
		server.Stop()
		listener.Close()
	}

	return capture, client, cleanup
}

// TestLookupRPCParameters verifies that Lookup sends correct gRPC parameters
func TestLookupRPCParameters(t *testing.T) {
	tests := []struct {
		name           string
		lookupPath     string
		wantParentPath string
		wantName       string
	}{
		{
			name:           "root level entry",
			lookupPath:     "github_com",
			wantParentPath: "",
			wantName:       "github_com",
		},
		{
			name:           "nested entry",
			lookupPath:     "github_com/user/repo",
			wantParentPath: "github_com/user",
			wantName:       "repo",
		},
		{
			name:           "two levels",
			lookupPath:     "github_com/user",
			wantParentPath: "github_com",
			wantName:       "user",
		},
		{
			name:           "file with extension",
			lookupPath:     "dir/file.txt",
			wantParentPath: "dir",
			wantName:       "file.txt",
		},
		{
			name:           "deeply nested",
			lookupPath:     "a/b/c/d/file.go",
			wantParentPath: "a/b/c/d",
			wantName:       "file.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, client, cleanup := setupTestServer(t)
			defer cleanup()

			_, err := client.Lookup(context.Background(), tt.lookupPath)
			if err != nil {
				t.Fatalf("Lookup failed: %v", err)
			}

			if !capture.called {
				t.Fatal("Server Lookup was not called")
			}

			if capture.lastParentPath != tt.wantParentPath {
				t.Errorf("ParentPath = %q, want %q", capture.lastParentPath, tt.wantParentPath)
			}

			if capture.lastName != tt.wantName {
				t.Errorf("Name = %q, want %q", capture.lastName, tt.wantName)
			}
		})
	}
}

// TestLookupEmptyPath verifies edge case of empty path
func TestLookupEmptyPath(t *testing.T) {
	capture, client, cleanup := setupTestServer(t)
	defer cleanup()

	_, err := client.Lookup(context.Background(), "")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	if !capture.called {
		t.Fatal("Server Lookup was not called")
	}

	// Empty path should send both as empty
	if capture.lastParentPath != "" {
		t.Errorf("ParentPath = %q, want empty", capture.lastParentPath)
	}
	if capture.lastName != "" {
		t.Errorf("Name = %q, want empty", capture.lastName)
	}
}
