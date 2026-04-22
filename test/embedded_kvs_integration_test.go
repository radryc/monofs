package test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/server"
	kvsv1 "github.com/rydzu/ainfra/kvs/api/proto/kvs/v1"
	kvsgrpc "github.com/rydzu/ainfra/kvs/pkg/grpcserver"
	"github.com/rydzu/ainfra/kvs/pkg/raftstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type embeddedKVSServerTestEnv struct {
	t          *testing.T
	server     *server.Server
	grpcServer *grpc.Server
	listener   net.Listener
	conn       *grpc.ClientConn
	client     pb.MonoFSClient
	kvsClient  kvsv1.KVStoreClient
	baseDir    string
	stopOnce   sync.Once
}

func newEmbeddedKVSServerTestEnv(t *testing.T, nodeID string) *embeddedKVSServerTestEnv {
	t.Helper()

	baseDir := t.TempDir()
	dbPath := filepath.Join(baseDir, "db")
	gitCache := filepath.Join(baseDir, "git")
	kvsDir := filepath.Join(baseDir, "kvs")

	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("failed to create db dir: %v", err)
	}
	if err := os.MkdirAll(gitCache, 0o755); err != nil {
		t.Fatalf("failed to create git cache dir: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	apiAddr := listener.Addr().String()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := server.NewServer(nodeID, apiAddr, dbPath, gitCache, logger)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("failed to create server: %v", err)
	}

	store, err := raftstore.Open(raftstore.Config{
		NodeID:         nodeID,
		DataDir:        kvsDir,
		RaftAddress:    allocateFreeTCPAddr(t),
		APIAddress:     apiAddr,
		Bootstrap:      true,
		MaxHotVersions: 2,
		LogOutput:      io.Discard,
	})
	if err != nil {
		_ = listener.Close()
		_ = srv.Close()
		t.Fatalf("failed to create embedded kvs store: %v", err)
	}
	srv.SetKVSStore(store)

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(256*1024*1024),
		grpc.MaxSendMsgSize(256*1024*1024),
	)
	kvsgrpc.Register(grpcServer, store, kvsgrpc.Config{})
	srv.Register(grpcServer)

	go func() { _ = grpcServer.Serve(listener) }()

	conn, err := grpc.Dial(apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()
		_ = srv.Close()
		t.Fatalf("failed to connect: %v", err)
	}

	return &embeddedKVSServerTestEnv{
		t:          t,
		server:     srv,
		grpcServer: grpcServer,
		listener:   listener,
		conn:       conn,
		client:     pb.NewMonoFSClient(conn),
		kvsClient:  kvsv1.NewKVStoreClient(conn),
		baseDir:    baseDir,
	}
}

func (env *embeddedKVSServerTestEnv) Close() {
	env.stopOnce.Do(func() {
		if env.conn != nil {
			_ = env.conn.Close()
		}
		if env.grpcServer != nil {
			env.grpcServer.Stop()
		}
		if env.listener != nil {
			_ = env.listener.Close()
		}
		if env.server != nil {
			_ = env.server.Close()
		}
	})
}

func allocateFreeTCPAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free tcp addr: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func ingestKVSBatchEventually(t *testing.T, client pb.MonoFSClient, req *pb.IngestFileBatchRequest) *pb.IngestFileBatchResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := client.IngestFileBatch(ctx, req)
		cancel()
		if err == nil {
			if resp != nil && resp.Success {
				return resp
			}
			if resp != nil && !looksLikeTransientLeaderError(resp.GetErrorMessage()) {
				t.Fatalf("ingest batch failed permanently: %+v", resp)
			}
		} else if !looksLikeTransientLeaderError(err.Error()) {
			t.Fatalf("ingest batch failed permanently: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out waiting for embedded kvs store to become writable")
	return nil
}

func looksLikeTransientLeaderError(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "not leader") ||
		strings.Contains(message, "is not the leader") ||
		strings.Contains(message, "leadership lost")
}

func readAllFromMonoFSStream(t *testing.T, client pb.MonoFSClient, fullPath string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Read(ctx, &pb.ReadRequest{Path: fullPath})
	if err != nil {
		t.Fatalf("Read(%q) error = %v", fullPath, err)
	}

	var content []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return content
		}
		if err != nil {
			t.Fatalf("Read(%q).Recv() error = %v", fullPath, err)
		}
		content = append(content, chunk.GetData()...)
	}
}

func collectDirEntries(t *testing.T, client pb.MonoFSClient, path string) map[string]*pb.DirEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.ReadDir(ctx, &pb.ReadDirRequest{Path: path})
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", path, err)
	}

	entries := make(map[string]*pb.DirEntry)
	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("ReadDir(%q).Recv() error = %v", path, err)
		}
		entries[entry.GetName()] = entry
	}
}

func TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC(t *testing.T) {
	env := newEmbeddedKVSServerTestEnv(t, "embedded-kvs-node")
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repos := []struct {
		storageID   string
		displayPath string
		filePath    string
		fullPath    string
		logicalPath string
		content     string
	}{
		{
			storageID:   "guardian-genomics-grpc",
			displayPath: "guardian/genomics",
			filePath:    "intents/api.yaml",
			fullPath:    "guardian/genomics/intents/api.yaml",
			logicalPath: "/guardian/genomics/intents/api.yaml",
			content:     "kind: Intent\nmetadata:\n  name: api\n",
		},
		{
			storageID:   "guardian-payments-grpc",
			displayPath: "guardian/payments",
			filePath:    "intents/worker.yaml",
			fullPath:    "guardian/payments/intents/worker.yaml",
			logicalPath: "/guardian/payments/intents/worker.yaml",
			content:     "kind: Intent\nmetadata:\n  name: worker\n",
		},
		{
			storageID:   "guardian-system-grpc",
			displayPath: "guardian-system",
			filePath:    ".queues/local/tasks/task-42.json",
			fullPath:    "guardian-system/.queues/local/tasks/task-42.json",
			logicalPath: "/guardian-system/.queues/local/tasks/task-42.json",
			content:     "{\"id\":\"task-42\"}",
		},
	}

	for _, repo := range repos {
		resp, err := env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:       repo.storageID,
			DisplayPath:     repo.displayPath,
			Source:          repo.displayPath,
			FetchConfig:     map[string]string{"storage_backend": "kvs"},
			IngestionConfig: map[string]string{"storage_backend": "kvs"},
		})
		if err != nil {
			t.Fatalf("RegisterRepository(%q) error = %v", repo.displayPath, err)
		}
		if !resp.GetSuccess() {
			t.Fatalf("RegisterRepository(%q) failed: %+v", repo.displayPath, resp)
		}

		batchResp := ingestKVSBatchEventually(t, env.client, &pb.IngestFileBatchRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      repo.displayPath,
			Files: []*pb.FileMetadata{{
				Path:          repo.filePath,
				StorageId:     repo.storageID,
				DisplayPath:   repo.displayPath,
				Size:          uint64(len(repo.content)),
				Mode:          0o644,
				Mtime:         time.Now().Unix(),
				InlineContent: []byte(repo.content),
				BackendMetadata: map[string]string{
					"storage_backend": "kvs",
				},
			}},
		})
		if batchResp.GetFilesIngested() != 1 {
			t.Fatalf("expected one file ingested for %q, got %+v", repo.displayPath, batchResp)
		}
	}

	guardianEntries := collectDirEntries(t, env.client, "guardian")
	if _, ok := guardianEntries["genomics"]; !ok {
		t.Fatalf("guardian root missing genomics: %+v", guardianEntries)
	}
	if _, ok := guardianEntries["payments"]; !ok {
		t.Fatalf("guardian root missing payments: %+v", guardianEntries)
	}

	queueEntries := collectDirEntries(t, env.client, "guardian-system/.queues/local/tasks")
	if _, ok := queueEntries["task-42.json"]; !ok {
		t.Fatalf("guardian-system queue dir missing task-42.json: %+v", queueEntries)
	}

	for _, repo := range repos {
		attr, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: repo.fullPath})
		if err != nil {
			t.Fatalf("GetAttr(%q) error = %v", repo.fullPath, err)
		}
		if !attr.GetFound() || attr.GetSize() != uint64(len(repo.content)) {
			t.Fatalf("unexpected GetAttr(%q) response: %+v", repo.fullPath, attr)
		}

		if content := string(readAllFromMonoFSStream(t, env.client, repo.fullPath)); content != repo.content {
			t.Fatalf("Read(%q) = %q, want %q", repo.fullPath, content, repo.content)
		}

		readResp, err := env.kvsClient.ReadFile(ctx, &kvsv1.ReadFileRequest{LogicalPath: repo.logicalPath})
		if err != nil {
			t.Fatalf("KVS ReadFile(%q) error = %v", repo.logicalPath, err)
		}
		if string(readResp.GetContent()) != repo.content {
			t.Fatalf("KVS ReadFile(%q) = %q, want %q", repo.logicalPath, string(readResp.GetContent()), repo.content)
		}
	}

	deleteResp, err := env.client.DeleteRepository(ctx, &pb.DeleteRepositoryOnNodeRequest{StorageId: repos[0].storageID})
	if err != nil {
		t.Fatalf("DeleteRepository(%q) error = %v", repos[0].displayPath, err)
	}
	if !deleteResp.GetSuccess() || deleteResp.GetFilesDeleted() != 1 {
		t.Fatalf("unexpected DeleteRepository response: %+v", deleteResp)
	}

	deletedAttr, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: repos[0].fullPath})
	if err != nil {
		t.Fatalf("GetAttr(%q) after delete error = %v", repos[0].fullPath, err)
	}
	if deletedAttr.GetFound() {
		t.Fatalf("expected %q to disappear after delete", repos[0].fullPath)
	}

	_, err = env.kvsClient.ReadFile(ctx, &kvsv1.ReadFileRequest{LogicalPath: repos[0].logicalPath})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected deleted kvs path %q to return not found, got %v", repos[0].logicalPath, err)
	}

	guardianEntries = collectDirEntries(t, env.client, "guardian")
	if _, ok := guardianEntries["genomics"]; ok {
		t.Fatalf("expected genomics to disappear from guardian root after delete: %+v", guardianEntries)
	}
	if _, ok := guardianEntries["payments"]; !ok {
		t.Fatalf("expected payments to remain in guardian root after deleting genomics: %+v", guardianEntries)
	}

	for _, repo := range repos[1:] {
		attr, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: repo.fullPath})
		if err != nil {
			t.Fatalf("GetAttr(%q) after unrelated delete error = %v", repo.fullPath, err)
		}
		if !attr.GetFound() {
			t.Fatalf("expected %q to remain after unrelated repo delete", repo.fullPath)
		}

		readResp, err := env.kvsClient.ReadFile(ctx, &kvsv1.ReadFileRequest{LogicalPath: repo.logicalPath})
		if err != nil {
			t.Fatalf("KVS ReadFile(%q) after unrelated delete error = %v", repo.logicalPath, err)
		}
		if string(readResp.GetContent()) != repo.content {
			t.Fatalf("unexpected surviving KVS content for %q: %q", repo.logicalPath, string(readResp.GetContent()))
		}
	}
}
