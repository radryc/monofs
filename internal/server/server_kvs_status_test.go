package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/kvs/pkg/localstore"
)

func TestGetNodeInfoReportsEmbeddedKVSStatus(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-node-kvs-status-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	server, err := NewServer("node-a", "localhost:9000", filepath.Join(tmpDir, "db"), filepath.Join(tmpDir, "git-cache"), nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	store, err := localstore.Open(localstore.Config{DataDir: filepath.Join(tmpDir, "kvs"), MaxHotVersions: 2})
	if err != nil {
		t.Fatalf("failed to open kvs store: %v", err)
	}
	server.SetKVSStore(store)

	resp, err := server.GetNodeInfo(context.Background(), &pb.NodeInfoRequest{})
	if err != nil {
		t.Fatalf("GetNodeInfo failed: %v", err)
	}
	if resp.GetKvs() == nil {
		t.Fatal("expected kvs status in node info response")
	}
	if !resp.GetKvs().GetEnabled() {
		t.Fatal("expected kvs status to report enabled")
	}
	if !resp.GetKvs().GetHealthy() {
		t.Fatal("expected local kvs to report healthy")
	}
	if got := resp.GetKvs().GetMode(); got != "local" {
		t.Fatalf("expected local kvs mode, got %q", got)
	}
	if got := resp.GetKvs().GetRole(); got != "local" {
		t.Fatalf("expected local kvs role, got %q", got)
	}
	if got := resp.GetKvs().GetPeerCount(); got != 1 {
		t.Fatalf("expected single-node local kvs peer count, got %d", got)
	}
}
