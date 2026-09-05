package fuse

import (
	"context"
	"encoding/json"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	pb "github.com/radryc/monofs/api/proto"
	monoclient "github.com/radryc/monofs/internal/client"
)

type workspaceViewMockClient struct {
	*pathMockClient
	workspaceRepos []monoclient.WorkspaceRepository
}

func (m *workspaceViewMockClient) ListWorkspaceRepositories(ctx context.Context) ([]monoclient.WorkspaceRepository, error) {
	return append([]monoclient.WorkspaceRepository(nil), m.workspaceRepos...), nil
}

func (m *workspaceViewMockClient) ResolveWorkspacePath(ctx context.Context, path string) (*monoclient.WorkspaceRepository, error) {
	trimmed := strings.Trim(path, "/")
	var match *monoclient.WorkspaceRepository
	for i := range m.workspaceRepos {
		repo := m.workspaceRepos[i]
		if trimmed != repo.DisplayPath && !strings.HasPrefix(trimmed, repo.DisplayPath+"/") {
			continue
		}
		if match == nil || len(repo.DisplayPath) > len(match.DisplayPath) {
			candidate := repo
			match = &candidate
		}
	}
	if match == nil {
		return nil, monoclient.ErrWorkspacePathNotFound
	}
	return match, nil
}

func TestVirtualMonorepoSystemViewExposesHiddenNamespaces(t *testing.T) {
	client := newWorkspaceViewMockClient()
	root := NewRoot(client, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("root Readdir() errno = %v", errno)
	}
	names := collectDirEntryNames(stream)
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, syntheticWorkspaceControlDirName) {
		t.Fatalf("root listing missing %q: %v", syntheticWorkspaceControlDirName, names)
	}
	if !strings.Contains(joined, "dependency") {
		t.Fatalf("root listing missing visible dependency namespace: %v", names)
	}
	if strings.Contains(joined, "doctor") || strings.Contains(joined, "guardian") {
		t.Fatalf("hidden namespaces leaked into workspace root: %v", names)
	}

	controlNode := root.newChild(syntheticWorkspaceControlDirName, true, 0555|uint32(syscall.S_IFDIR), 0)
	stream, errno = controlNode.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("control Readdir() errno = %v", errno)
	}
	names = collectDirEntryNames(stream)
	joined = strings.Join(names, ",")
	if !strings.Contains(joined, syntheticWorkspaceSystemDirName) || !strings.Contains(joined, syntheticWorkspaceManifestName) {
		t.Fatalf("control listing missing expected entries: %v", names)
	}

	systemNode := controlNode.newChild(syntheticWorkspaceSystemDirName, true, 0555|uint32(syscall.S_IFDIR), 0)
	stream, errno = systemNode.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("system Readdir() errno = %v", errno)
	}
	names = collectDirEntryNames(stream)
	joined = strings.Join(names, ",")
	for _, want := range []string{"github.com", "dependency", "guardian", "doctor"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("system view missing %q: %v", want, names)
		}
	}

	guardianNode := systemNode.newChild("guardian", true, 0555|uint32(syscall.S_IFDIR), 0)
	var attrOut fuse.AttrOut
	if errno := guardianNode.Getattr(context.Background(), nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr(.monofs/system/guardian) errno = %v", errno)
	}
	if attrOut.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Fatalf("Getattr(.monofs/system/guardian) mode = %#o, want directory", attrOut.Mode)
	}
}

func TestVirtualMonorepoWorkspaceManifestFile(t *testing.T) {
	client := newWorkspaceViewMockClient()
	root := NewRoot(client, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	controlNode := root.newChild(syntheticWorkspaceControlDirName, true, 0555|uint32(syscall.S_IFDIR), 0)
	manifestNode := controlNode.newChild(syntheticWorkspaceManifestName, false, 0444|uint32(syscall.S_IFREG), 0)

	var attrOut fuse.AttrOut
	if errno := manifestNode.Getattr(context.Background(), nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr(.monofs/workspace.json) errno = %v", errno)
	}
	if attrOut.Size == 0 {
		t.Fatal("workspace manifest file should not be empty")
	}
	if _, _, errno := manifestNode.Open(context.Background(), 0); errno != 0 {
		t.Fatalf("Open(.monofs/workspace.json) errno = %v", errno)
	}

	var doc struct {
		Repositories []struct {
			DisplayPath     string `json:"display_path"`
			Included        bool   `json:"included"`
			ExclusionReason string `json:"exclusion_reason"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(manifestNode.content, &doc); err != nil {
		t.Fatalf("workspace manifest JSON unmarshal failed: %v", err)
	}
	if len(doc.Repositories) == 0 {
		t.Fatal("workspace manifest JSON should list repositories")
	}

	foundSource := false
	foundDependencyExcluded := false
	foundDoctorExcluded := false
	for _, repo := range doc.Repositories {
		switch repo.DisplayPath {
		case "github.com/acme/monofs":
			foundSource = repo.Included
		case "dependency/go/mod/cache":
			foundDependencyExcluded = !repo.Included && repo.ExclusionReason == string(WorkspaceExcludedSystemNamespace)
		case "doctor/v1":
			foundDoctorExcluded = !repo.Included && repo.ExclusionReason == string(WorkspaceExcludedSystemNamespace)
		}
	}
	if !foundSource {
		t.Fatal("workspace manifest JSON missing included source repo")
	}
	if !foundDependencyExcluded {
		t.Fatal("workspace manifest JSON missing excluded dependency repo")
	}
	if !foundDoctorExcluded {
		t.Fatal("workspace manifest JSON missing excluded doctor namespace repo")
	}
}

func TestVirtualMonorepoSystemViewIsReadOnly(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	root := NewRootWithSession(newWorkspaceViewMockClient(), nil, sessionMgr, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	controlNode := root.newChild(syntheticWorkspaceControlDirName, true, 0555|uint32(syscall.S_IFDIR), 0)
	systemNode := controlNode.newChild(syntheticWorkspaceSystemDirName, true, 0555|uint32(syscall.S_IFDIR), 0)

	var entryOut fuse.EntryOut
	if _, errno := systemNode.Mkdir(context.Background(), "tmp", 0755, &entryOut); errno != syscall.EROFS {
		t.Fatalf("Mkdir(.monofs/system/tmp) errno = %v, want %v", errno, syscall.EROFS)
	}
	if _, _, _, errno := systemNode.Create(context.Background(), "tmp.txt", 0, 0644, &entryOut); errno != syscall.EROFS {
		t.Fatalf("Create(.monofs/system/tmp.txt) errno = %v, want %v", errno, syscall.EROFS)
	}
	fileNode := systemNode.newChild("dependency/file.txt", false, 0644|uint32(syscall.S_IFREG), 0)
	if _, _, errno := fileNode.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("Open(.monofs/system/dependency/file.txt, O_WRONLY) errno = %v, want %v", errno, syscall.EROFS)
	}
	if errno := systemNode.Unlink(context.Background(), "dependency/file.txt"); errno != syscall.EROFS {
		t.Fatalf("Unlink(.monofs/system/dependency/file.txt) errno = %v, want %v", errno, syscall.EROFS)
	}
}

func newWorkspaceViewMockClient() *workspaceViewMockClient {
	return &workspaceViewMockClient{
		pathMockClient: &pathMockClient{
			readdirFunc: func(ctx context.Context, path string) ([]*pb.DirEntry, error) {
				switch path {
				case "":
					return []*pb.DirEntry{
						{Name: "github.com", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 1},
						{Name: "dependency", Mode: 0555 | uint32(syscall.S_IFDIR), Ino: 2},
						{Name: "guardian", Mode: 0555 | uint32(syscall.S_IFDIR), Ino: 3},
						{Name: "doctor", Mode: 0555 | uint32(syscall.S_IFDIR), Ino: 4},
					}, nil
				default:
					return nil, nil
				}
			},
			getattrFunc: func(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
				switch path {
				case "dependency", "guardian", "doctor", "github.com":
					return &pb.GetAttrResponse{
						Found: true,
						Ino:   1,
						Mode:  0555 | uint32(syscall.S_IFDIR),
						Size:  0,
						Nlink: 2,
						Uid:   1000,
						Gid:   1000,
					}, nil
				default:
					return &pb.GetAttrResponse{Found: false}, nil
				}
			},
		},
		workspaceRepos: []monoclient.WorkspaceRepository{
			{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "git@example/monofs", Ref: "main", CommitHash: "abc123"},
			{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian", Source: "git@example/guardian", Ref: "main", CommitHash: "def456"},
			{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor", Source: "git@example/doctor", Ref: "main", CommitHash: "ghi789"},
			{StorageID: "doctor-managed", DisplayPath: "doctor/v1", Source: "system:doctor"},
			{StorageID: "dep-cache", DisplayPath: "dependency/go/mod/cache", Source: "system:dependency"},
		},
	}
}
