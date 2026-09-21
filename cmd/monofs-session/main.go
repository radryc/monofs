// MonoFS Session - Write session management CLI for MonoFS
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultSocketTimeout = 30 * time.Second
	pushSocketTimeout    = 10 * time.Minute
)

// SessionRequest is sent to the FUSE client
type SessionRequest struct {
	Action                  string   `json:"action"` // start, add, rm, status, branch, log, commit, pull, discard, push, push-blobs, diff
	Path                    string   `json:"path,omitempty"`
	Paths                   []string `json:"paths,omitempty"`
	BranchOp                string   `json:"branch_op,omitempty"`
	BranchName              string   `json:"branch_name,omitempty"`
	ShowBlobs               bool     `json:"show_blobs,omitempty"`
	Upstream                bool     `json:"upstream,omitempty"`
	RepoFilter              string   `json:"repo_filter,omitempty"`
	Limit                   int      `json:"limit,omitempty"`
	LogicalCommitMessage    string   `json:"logical_commit_message,omitempty"`
	AuthorName              string   `json:"author_name,omitempty"`
	AuthorEmail             string   `json:"author_email,omitempty"`
	RequestedBranchStrategy string   `json:"requested_branch_strategy,omitempty"`
	SourcePushMode          string   `json:"source_push_mode,omitempty"` // squash or preserve (empty = router default)
}

// FileDiff holds the unified diff output for a single changed file.
type FileDiff struct {
	Path       string `json:"path"`
	ChangeType string `json:"change_type"`
	Repository string `json:"repository,omitempty"`
	StorageID  string `json:"storage_id,omitempty"`
	Diff       string `json:"diff"`
}

// SessionResponse is received from the FUSE client
type SessionResponse struct {
	Success           bool                `json:"success"`
	SessionID         string              `json:"session_id,omitempty"`
	CreatedAt         string              `json:"created_at,omitempty"`
	Changes           int                 `json:"changes,omitempty"`
	UnstagedChanges   int                 `json:"unstaged_changes,omitempty"`
	StagedChanges     int                 `json:"staged_changes,omitempty"`
	PendingCommits    int                 `json:"pending_commits,omitempty"`
	BlobChanges       int                 `json:"blob_changes,omitempty"`
	ExcludedChanges   int                 `json:"excluded_changes,omitempty"`
	Conflicts         int                 `json:"conflicts,omitempty"`
	ConflictList      []ConflictInfo      `json:"conflict_list,omitempty"`
	Message           string              `json:"message,omitempty"`
	Error             string              `json:"error,omitempty"`
	ChangeList        []ChangeInfo        `json:"change_list,omitempty"`
	StagedChangeList  []ChangeInfo        `json:"staged_change_list,omitempty"`
	LocalCommitList   []LocalCommitInfo   `json:"local_commit_list,omitempty"`
	PendingCommitList []LocalCommitInfo   `json:"pending_commit_list,omitempty"`
	CurrentBranch     string              `json:"current_branch,omitempty"`
	BranchList        []BranchInfo        `json:"branch_list,omitempty"`
	BranchMappings    []BranchMappingInfo `json:"branch_mappings,omitempty"`
	BlobChangeList    []ChangeInfo        `json:"blob_change_list,omitempty"`
	WorkspaceRefs     []WorkspaceRef      `json:"workspace_refs,omitempty"`
	DepsInfo          *BlobsInfoData      `json:"blobs_info,omitempty"`
	DiffData          []FileDiff          `json:"diff_data,omitempty"`
	BlobDiffData      []FileDiff          `json:"blob_diff_data,omitempty"`
	UpstreamLogList   []UpstreamLogInfo   `json:"upstream_log_list,omitempty"`
	TagList           []UpstreamTagInfo   `json:"tag_list,omitempty"`
	BlameLines        []UpstreamBlameInfo `json:"blame_lines,omitempty"`
}

// UpstreamLogInfo is one commit in an upstream branch's history.
type UpstreamLogInfo struct {
	Hash        string `json:"hash"`
	AuthorName  string `json:"author_name,omitempty"`
	AuthorEmail string `json:"author_email,omitempty"`
	AuthoredAt  string `json:"authored_at,omitempty"`
	Message     string `json:"message,omitempty"`
}

// UpstreamTagInfo is one upstream tag.
type UpstreamTagInfo struct {
	Name       string `json:"name"`
	CommitHash string `json:"commit_hash"`
	TaggedAt   string `json:"tagged_at,omitempty"`
}

// UpstreamBlameInfo is one blamed line of a file.
type UpstreamBlameInfo struct {
	LineNo     int    `json:"line_no"`
	Hash       string `json:"hash"`
	Author     string `json:"author,omitempty"`
	AuthoredAt string `json:"authored_at,omitempty"`
	Content    string `json:"content"`
}

// ConflictInfo represents one unresolved merge conflict for display.
type ConflictInfo struct {
	Path      string `json:"path"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// WorkspaceRef describes the authoritative tracked ref for one mounted repository.
type WorkspaceRef struct {
	DisplayPath string `json:"display_path"`
	Ref         string `json:"ref,omitempty"`
	CommitHash  string `json:"commit_hash,omitempty"`
}

// BlobsInfoData contains blob file information.
type BlobsInfoData struct {
	TotalFiles int             `json:"total_files"`
	TotalBytes int64           `json:"total_bytes"`
	Tools      []BlobsToolInfo `json:"tools"`
}

// BlobsToolInfo contains per-tool dependency information.
type BlobsToolInfo struct {
	Tool     string         `json:"tool"`
	Files    int            `json:"files"`
	Bytes    int64          `json:"bytes"`
	FileList []BlobFileInfo `json:"file_list,omitempty"`
}

// BlobFileInfo describes a single blob file.
type BlobFileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type setupEnvEntry struct {
	envVar string
	dir    string
}

// ChangeInfo represents a single change for display
type ChangeInfo struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Repository string `json:"repository,omitempty"`
	StorageID  string `json:"storage_id,omitempty"`
	Timestamp  string `json:"timestamp"`
}

type LocalCommitInfo struct {
	ID              string `json:"id"`
	ParentID        string `json:"parent_id,omitempty"`
	Message         string `json:"message"`
	LogicalBranch   string `json:"logical_branch,omitempty"`
	AuthorName      string `json:"author_name,omitempty"`
	AuthorEmail     string `json:"author_email,omitempty"`
	PrincipalID     string `json:"principal_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	RepositoryCount int    `json:"repository_count,omitempty"`
	OperationCount  int    `json:"operation_count,omitempty"`
	Pushed          bool   `json:"pushed,omitempty"`
}

type BranchInfo struct {
	Name           string `json:"name"`
	Current        bool   `json:"current,omitempty"`
	PendingCommits int    `json:"pending_commits,omitempty"`
	HasMappings    bool   `json:"has_mappings,omitempty"`
}

type BranchMappingInfo struct {
	DisplayPath      string `json:"display_path"`
	OriginalBranch   string `json:"original_branch,omitempty"`
	ActualBranch     string `json:"actual_branch,omitempty"`
	LastPushedCommit string `json:"last_pushed_commit,omitempty"`
}

// SessionCommand handles write session management via Unix socket
type SessionCommand struct {
	socketPath string
}

// NewSessionCommand creates a session command handler
func NewSessionCommand(overlayDir string) *SessionCommand {
	return &SessionCommand{
		socketPath: filepath.Join(overlayDir, "session.sock"),
	}
}

func defaultSessionSocketPath() string {
	overlayDir := firstNonEmpty(
		os.Getenv("MONOFS_OVERLAY_DIR"),
		os.Getenv("GITFS_OVERLAY_DIR"),
		os.Getenv("MONOFS_OVERLAY"),
	)
	if overlayDir != "" {
		return filepath.Join(overlayDir, "session.sock")
	}

	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".monofs", "overlay", "session.sock")
}

func main() {
	// Parse flags first
	socketPath := ""
	args := os.Args[1:]

	// Extract --socket flag if present
	for i := 0; i < len(args); i++ {
		if args[i] == "--socket" && i+1 < len(args) {
			socketPath = args[i+1]
			// Remove flag and value from args
			args = append(args[:i], args[i+2:]...)
			break
		} else if len(args[i]) > 9 && args[i][:9] == "--socket=" {
			socketPath = args[i][9:]
			args = append(args[:i], args[i+1:]...)
			break
		}
	}

	// Determine overlay directory for socket path
	if socketPath == "" {
		socketPath = defaultSessionSocketPath()
	}

	cmd := &SessionCommand{socketPath: socketPath}

	// Get command from args
	if err := cmd.Execute(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// Execute runs a session command
func (sc *SessionCommand) Execute(args []string) error {
	if len(args) < 1 {
		return sc.printUsage()
	}

	switch args[0] {
	case "start":
		return sc.startSession()
	case "add":
		return sc.addToIndex(args[1:])
	case "rm":
		return sc.removeFromIndex(args[1:])
	case "status":
		return sc.showStatus(args[1:])
	case "branch":
		return sc.manageBranches(args[1:])
	case "refs":
		return sc.showRefs(args[1:])
	case "log":
		return sc.showLog(args[1:])
	case "tags":
		return sc.showTags(args[1:])
	case "blame":
		return sc.showBlame(args[1:])
	case "commit":
		return sc.commitSession(args[1:])
	case "pull":
		return sc.pullWorkspace()
	case "conflicts":
		return sc.showConflicts()
	case "discard":
		return sc.discardSession()
	case "search":
		return sc.searchCode(args[1:])
	case "setup":
		return sc.setupBlobs(args[1:])
	case "diff":
		return sc.showDiff(args[1:])
	case "blobs-info":
		return sc.showBlobsInfo(args[1:])
	case "push":
		return sc.pushSource(args[1:])
	case "push-blobs", "push-deps":
		return sc.uploadBlobs(args[1:])
	case "help", "--help", "-h":
		return sc.printUsage()
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func (sc *SessionCommand) printUsage() error {
	fmt.Printf(`MonoFS Session - Write Session Management & Code Search

Usage: monofs-session [--socket <path>] <command>

Commands:
	Session lifecycle:
		start        Start a new write session (or show current if active)
		status       Show current session status and pending changes
		discard      Abandon all local changes and delete session

	Source changes:
		add          Stage source changes as local snapshots
		rm           Remove source paths and stage deletes
		diff [file]  Show unified diff between original and changed files
		commit       Create a local virtual commit from staged source changes
		pull         Re-ingest included workspace repositories from their upstream sources
		push         Send pending local virtual commits upstream (--push-mode squash|preserve)
		conflicts    List unresolved merge conflicts from a pull

	Workspace state:
		branch       Show, create, switch, or delete logical branches (branch delete <name>)
		refs         Show tracked workspace refs for the mounted repositories
		log          Show local virtual commit history (--upstream for the remote)

	Upstream reads:
		tags         List tags on a repository (--repo <display-path>)
		blame <path> Show line-level authorship for a file

	Blobs and search:
		blobs-info   Show blob files in the current session
		push-blobs   Package and upload blob files to storage backend
		push-deps    Alias for push-blobs
		search       Search code across indexed repositories
		setup        Create blob cache dirs on monofs and print env exports

	Misc:
		help         Show this help message

Options:
  --socket <path>  Explicit path to session socket file

Write sessions allow you to make local modifications, stage source snapshots,
and checkpoint them as local virtual commits before later push flows publish them upstream.

Environment:
	MONOFS_OVERLAY_DIR  Preferred overlay location override
	GITFS_OVERLAY_DIR   Legacy overlay location override still honored
	MONOFS_OVERLAY      Mount helper overlay path (fallback)

Examples:
  # Start a new session
  monofs-session start

  # Check what changes are pending
  monofs-session status

	# Stage source changes before commit
	monofs-session add github.com/acme/service-a/file.go
	monofs-session add github.com/acme/service-a github.com/acme/lib

	# Remove a file or directory and stage the delete
	monofs-session rm github.com/acme/service-a/file.go
	monofs-session rm github.com/acme/service-a/internal

	# Show authoritative tracked refs for the mounted repositories
	monofs-session refs

	# Show logical branch state
	monofs-session branch
	monofs-session branch show

	# Create or switch logical branches for future local commits
	monofs-session branch create feature/session-vcs
	monofs-session branch switch feature/session-vcs

	# Inspect local virtual commit history
	monofs-session log

  # Include blob file changes in status/diff
  monofs-session status --deps
  monofs-session diff --deps

  # Search for code
  monofs-session search --query "func main" --max-results 10

  # Search with filters
  monofs-session search --query "TODO" --regex --case-sensitive

  # Search for a symbol (function/type/class name; requires universal-ctags)
  monofs-session search --symbol "someFunction"

  # Use explicit socket path (useful in Docker)
  monofs-session --socket /path/to/session.sock status

	# Create a local virtual commit from staged changes
  monofs-session commit

	# Record author metadata on the local virtual commit
	monofs-session commit -m "Update API" --author-name "Dev" --author-email dev@example.com

	# Push pending local commits on the current logical branch upstream
	monofs-session push

	# Upload dependency blobs only, without pushing source/Git changes
	monofs-session push-deps

	# Refresh the virtual workspace from upstream sources
	monofs-session pull

  # Abandon all local changes
  monofs-session discard

  # Upload blob files to storage backend
  monofs-session push-blobs

  # Setup blob caches on monofs (eval to apply)
  eval $(monofs-session setup --mount /mnt/monofs)

  # Setup only specific package managers
  eval $(monofs-session setup --mount /mnt/monofs --tools go,cargo)

Current socket path: %s
`, sc.socketPath)
	return nil
}

func (sc *SessionCommand) sendCommand(action string) (*SessionResponse, error) {
	// Check if socket exists
	if _, err := os.Stat(sc.socketPath); os.IsNotExist(err) {
		homeDir, _ := os.UserHomeDir()
		return nil, fmt.Errorf(`session socket not found at %s

Make sure monofs-client is running with --writable flag.

Possible fixes:
  1. Start monofs-client with --writable flag:
     monofs-client --mount /mnt --writable

  2. Set MONOFS_OVERLAY_DIR or GITFS_OVERLAY_DIR to match monofs-client's --overlay path:
	  export MONOFS_OVERLAY_DIR=/path/to/overlay
     monofs-session status

  3. Use --socket to specify explicit path:
     monofs-session --socket /path/to/session.sock status

Common socket locations:
	- %s/.monofs/overlay/session.sock (default)
	- /tmp/monofs-overlay/session.sock (Docker common)
	- $GITFS_OVERLAY_DIR/session.sock (legacy dev-workspace env)

Run 'find / -name session.sock 2>/dev/null' to locate existing sockets.
`, sc.socketPath, homeDir)
	}

	// Connect to socket
	conn, err := net.Dial("unix", sc.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to session socket: %w", err)
	}
	defer conn.Close()

	// Set timeout
	conn.SetDeadline(time.Now().Add(socketTimeoutForAction(action)))

	// Send request
	req := SessionRequest{Action: action}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	var resp SessionResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &resp, nil
}

func (sc *SessionCommand) sendCommandWithPath(action, path string) (*SessionResponse, error) {
	return sc.sendRequest(SessionRequest{Action: action, Path: path})
}

// sendRequest sends an arbitrary SessionRequest to the FUSE daemon and returns the response.
func (sc *SessionCommand) sendRequest(req SessionRequest) (*SessionResponse, error) {
	// Check if socket exists
	if _, err := os.Stat(sc.socketPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("session socket not found at %s (is monofs-client running with --writable? check MONOFS_OVERLAY_DIR or GITFS_OVERLAY_DIR)", sc.socketPath)
	}

	conn, err := net.Dial("unix", sc.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to session socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(socketTimeoutForAction(req.Action)))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	var resp SessionResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &resp, nil
}

func socketTimeoutForAction(action string) time.Duration {
	switch action {
	case "commit", "push", "push-blobs", "pull":
		// Push keeps the socket open until upload, backend verification,
		// overlay cleanup finish, publish may push multiple repos, and pull
		// may re-ingest multiple repos.
		return pushSocketTimeout
	default:
		return defaultSocketTimeout
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// validatePushMode rejects push-mode values other than "", squash, preserve.
func validatePushMode(mode string) error {
	switch normalizePushMode(mode) {
	case "", "squash", "preserve":
		return nil
	default:
		return fmt.Errorf("invalid --push-mode %q (expected squash or preserve)", mode)
	}
}

// normalizePushMode canonicalizes a push-mode flag value.
func normalizePushMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

func (sc *SessionCommand) showDiff(args []string) error {
	diffCmd := flag.NewFlagSet("diff", flag.ExitOnError)
	showBlobs := diffCmd.Bool("deps", false, "Include blob file diffs")
	if err := diffCmd.Parse(args); err != nil {
		return err
	}

	var filterPath string
	if diffCmd.NArg() > 0 {
		filterPath = diffCmd.Arg(0)
	}

	resp, err := sc.sendRequest(SessionRequest{
		Action:    "diff",
		Path:      filterPath,
		ShowBlobs: *showBlobs,
	})
	if err != nil {
		return err
	}

	if !resp.Success {
		if resp.Error == "no active session" {
			fmt.Println("No active write session.")
			return nil
		}
		return fmt.Errorf("%s", resp.Error)
	}

	if resp.Message != "" {
		fmt.Println(resp.Message)
		return nil
	}

	for i, fd := range resp.DiffData {
		if i > 0 {
			fmt.Println()
		}
		if i == 0 || resp.DiffData[i-1].Repository != fd.Repository {
			if fd.Repository != "" {
				fmt.Printf("=== %s ===\n", fd.Repository)
			}
		}
		if fd.Diff != "" {
			fmt.Print(fd.Diff)
		}
	}

	if *showBlobs && len(resp.BlobDiffData) > 0 {
		if len(resp.DiffData) > 0 {
			fmt.Println()
		}
		fmt.Println("=== Dependency Diffs ===")
		for i, fd := range resp.BlobDiffData {
			if i > 0 {
				fmt.Println()
			}
			if i == 0 || resp.BlobDiffData[i-1].Repository != fd.Repository {
				if fd.Repository != "" {
					fmt.Printf("=== %s ===\n", fd.Repository)
				}
			}
			if fd.Diff != "" {
				fmt.Print(fd.Diff)
			}
		}
	}

	return nil
}

func (sc *SessionCommand) startSession() error {
	resp, err := sc.sendCommand("start")
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("failed to start session: %s", resp.Error)
	}

	fmt.Printf("✓ Write session active\n")
	fmt.Printf("  Session ID: %s\n", resp.SessionID)
	fmt.Printf("  Created:    %s\n", resp.CreatedAt)
	fmt.Printf("\nYou can now modify files in the mounted filesystem.\n")
	fmt.Printf("Use 'monofs-session status' to see pending changes.\n")
	fmt.Printf("Use 'monofs-session commit' to checkpoint staged source changes locally.\n")
	fmt.Printf("Use 'monofs-session log' to inspect local virtual commits.\n")

	return nil
}

func (sc *SessionCommand) addToIndex(args []string) error {
	addCmd := flag.NewFlagSet("add", flag.ExitOnError)
	if err := addCmd.Parse(args); err != nil {
		return err
	}
	if addCmd.NArg() == 0 {
		return fmt.Errorf("add requires at least one path")
	}

	paths := make([]string, 0, addCmd.NArg())
	for _, path := range addCmd.Args() {
		trimmed := strings.TrimSpace(path)
		if trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	if len(paths) == 0 {
		return fmt.Errorf("add requires at least one non-empty path")
	}

	resp, err := sc.sendRequest(SessionRequest{Action: "add", Paths: paths})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to stage changes: %s", resp.Error)
	}

	if resp.Message != "" {
		fmt.Println(resp.Message)
	} else {
		fmt.Printf("Staged %d source change(s)\n", resp.StagedChanges)
	}
	printChangeInfoSection("Staged Changes:", resp.StagedChangeList)
	return nil
}

func (sc *SessionCommand) removeFromIndex(args []string) error {
	rmCmd := flag.NewFlagSet("rm", flag.ExitOnError)
	if err := rmCmd.Parse(args); err != nil {
		return err
	}
	if rmCmd.NArg() == 0 {
		return fmt.Errorf("rm requires at least one path")
	}

	paths := make([]string, 0, rmCmd.NArg())
	for _, path := range rmCmd.Args() {
		trimmed := strings.TrimSpace(path)
		if trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	if len(paths) == 0 {
		return fmt.Errorf("rm requires at least one non-empty path")
	}

	resp, err := sc.sendRequest(SessionRequest{Action: "rm", Paths: paths})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to remove paths: %s", resp.Error)
	}

	if resp.Message != "" {
		fmt.Println(resp.Message)
	} else {
		fmt.Printf("Removed %d path(s)\n", resp.Changes)
	}
	printChangeInfoSection("Staged Changes:", resp.StagedChangeList)
	return nil
}

func (sc *SessionCommand) showStatus(args []string) error {
	statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
	showBlobs := statusCmd.Bool("deps", false, "Show individual blob file changes")
	if err := statusCmd.Parse(args); err != nil {
		return err
	}

	resp, err := sc.sendRequest(SessionRequest{
		Action:    "status",
		ShowBlobs: *showBlobs,
	})
	if err != nil {
		return err
	}

	if !resp.Success {
		if resp.Error == "no active session" {
			fmt.Println("No active write session.")
			fmt.Println("\nUse 'monofs-session start' to begin a new session.")
			return nil
		}
		return fmt.Errorf("failed to get status: %s", resp.Error)
	}

	fmt.Printf("Write Session Status\n")
	fmt.Printf("====================\n")
	fmt.Printf("Session ID: %s\n", resp.SessionID)
	fmt.Printf("Created:    %s\n", resp.CreatedAt)
	fmt.Printf("Workspace:  %d file(s)\n", resp.Changes)
	fmt.Printf("Unstaged:   %d file(s)\n", resp.UnstagedChanges)
	fmt.Printf("Staged:     %d file(s)\n", resp.StagedChanges)
	fmt.Printf("Pending:    %d local commit(s)\n", resp.PendingCommits)
	fmt.Printf("Authority:  monofs-session is the source of truth; root git is synthetic\n")
	if resp.BlobChanges > 0 {
		if *showBlobs {
			fmt.Printf("Blobs:       %d file(s)  (use 'push-deps' or 'push-blobs' to upload)\n", resp.BlobChanges)
		} else {
			fmt.Printf("Blobs:       %d file(s)  (use --deps to show, 'push-deps' or 'push-blobs' to upload)\n", resp.BlobChanges)
		}
	}
	if resp.ExcludedChanges > 0 {
		fmt.Printf("Excluded:    %d path(s) outside virtual monorepo\n", resp.ExcludedChanges)
	}
	fmt.Println()

	if len(resp.ChangeList) > 0 {
		printChangeInfoSection("Unstaged Changes:", resp.ChangeList)
	}

	if len(resp.StagedChangeList) > 0 {
		if len(resp.ChangeList) > 0 {
			fmt.Println()
		}
		printChangeInfoSection("Staged Changes:", resp.StagedChangeList)
	}

	if len(resp.PendingCommitList) > 0 {
		if len(resp.ChangeList) > 0 || len(resp.StagedChangeList) > 0 {
			fmt.Println()
		}
		fmt.Println("Pending Local Commits:")
		for _, commit := range resp.PendingCommitList {
			branch := firstNonEmpty(strings.TrimSpace(commit.LogicalBranch), "(default)")
			fmt.Printf("  %s  %s  [%s]\n", commit.ID, commit.Message, branch)
		}
	} else if resp.BlobChanges == 0 && len(resp.StagedChangeList) == 0 {
		fmt.Println("No changes yet.")
	}

	if *showBlobs && len(resp.BlobChangeList) > 0 {
		fmt.Println()
		fmt.Println("Dependency Changes:")
		lastRepo := ""
		for _, change := range resp.BlobChangeList {
			if change.Repository != "" && change.Repository != lastRepo {
				fmt.Printf("  %s\n", change.Repository)
				lastRepo = change.Repository
			}
			symbol := getChangeSymbol(change.Type)
			indent := "  "
			if change.Repository != "" {
				indent = "    "
			}
			fmt.Printf("%s%s %s\n", indent, symbol, change.Path)
		}
	}

	return nil
}

func (sc *SessionCommand) showLog(args []string) error {
	logCmd := flag.NewFlagSet("log", flag.ExitOnError)
	upstream := logCmd.Bool("upstream", false, "Show upstream commit history instead of local commits")
	repoFilter := logCmd.String("repo", "", "Repository display path to query (upstream only)")
	limit := logCmd.Int("limit", 50, "Maximum number of commits (upstream only)")
	if err := logCmd.Parse(args); err != nil {
		return err
	}

	if *upstream {
		resp, err := sc.sendRequest(SessionRequest{Action: "log", Upstream: true, RepoFilter: *repoFilter, Limit: *limit})
		if err != nil {
			return err
		}
		return displayUpstreamLog(resp)
	}

	resp, err := sc.sendCommand("log")
	if err != nil {
		return err
	}

	if !resp.Success {
		if resp.Error == "no active session" {
			fmt.Println("No active write session.")
			fmt.Println("\nUse 'monofs-session start' to begin a new session.")
			return nil
		}
		return fmt.Errorf("failed to get local commit log: %s", resp.Error)
	}

	fmt.Printf("Local Commit Log\n")
	fmt.Printf("================\n")
	fmt.Printf("Session ID: %s\n", resp.SessionID)
	fmt.Printf("Created:    %s\n", resp.CreatedAt)
	fmt.Printf("Authority:  monofs-session is the source of truth; root git is synthetic\n")

	if len(resp.LocalCommitList) == 0 {
		fmt.Println()
		fmt.Println("No local commits yet.")
		return nil
	}

	for i, commit := range resp.LocalCommitList {
		if i == 0 {
			fmt.Println()
		} else {
			fmt.Println()
			fmt.Println()
		}
		fmt.Printf("commit %s\n", commit.ID)
		fmt.Printf("Created:    %s\n", commit.CreatedAt)
		fmt.Printf("Branch:     %s\n", firstNonEmpty(strings.TrimSpace(commit.LogicalBranch), "(default)"))
		if parentID := strings.TrimSpace(commit.ParentID); parentID != "" {
			fmt.Printf("Parent:     %s\n", parentID)
		}
		fmt.Printf("Principal:  %s\n", firstNonEmpty(strings.TrimSpace(commit.PrincipalID), "(unknown)"))
		if author := formatLocalCommitAuthor(commit); author != "" {
			fmt.Printf("Author:     %s\n", author)
		}
		fmt.Printf("State:      %s\n", localCommitState(commit))
		fmt.Printf("Changes:    %d operation(s) across %d repo(s)\n", commit.OperationCount, commit.RepositoryCount)
		fmt.Printf("\n    %s\n", commit.Message)
	}

	return nil
}

func displayUpstreamLog(resp *SessionResponse) error {
	if !resp.Success {
		return fmt.Errorf("failed to read upstream log: %s", resp.Error)
	}
	fmt.Printf("Upstream Commit Log\n")
	fmt.Printf("===================\n")
	if len(resp.UpstreamLogList) == 0 {
		fmt.Println("\nNo upstream commits.")
		return nil
	}
	for _, e := range resp.UpstreamLogList {
		fmt.Println()
		fmt.Printf("commit %s\n", e.Hash)
		if e.AuthoredAt != "" {
			fmt.Printf("Date:    %s\n", e.AuthoredAt)
		}
		if e.AuthorName != "" {
			fmt.Printf("Author:  %s <%s>\n", e.AuthorName, e.AuthorEmail)
		}
		if e.Message != "" {
			fmt.Printf("\n    %s\n", e.Message)
		}
	}
	return nil
}

func (sc *SessionCommand) showTags(args []string) error {
	tagsCmd := flag.NewFlagSet("tags", flag.ExitOnError)
	repoFilter := tagsCmd.String("repo", "", "Repository display path")
	if err := tagsCmd.Parse(args); err != nil {
		return err
	}

	resp, err := sc.sendRequest(SessionRequest{Action: "tags", RepoFilter: *repoFilter})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to list upstream tags: %s", resp.Error)
	}

	fmt.Printf("Upstream Tags\n")
	fmt.Printf("=============\n")
	if len(resp.TagList) == 0 {
		fmt.Println("\nNo tags.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TAG\tCOMMIT\tTAGGED")
	for _, t := range resp.TagList {
		fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, shortCommitHash(t.CommitHash), t.TaggedAt)
	}
	return w.Flush()
}

func (sc *SessionCommand) showBlame(args []string) error {
	blameCmd := flag.NewFlagSet("blame", flag.ExitOnError)
	if err := blameCmd.Parse(args); err != nil {
		return err
	}
	path := strings.TrimSpace(blameCmd.Arg(0))
	if path == "" {
		return fmt.Errorf("blame requires a file path")
	}

	resp, err := sc.sendRequest(SessionRequest{Action: "blame", Path: path})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to blame %s: %s", path, resp.Error)
	}

	for _, l := range resp.BlameLines {
		fmt.Printf("%s %4d  %-20s %s\n", shortCommitHash(l.Hash), l.LineNo, l.Author, l.Content)
	}
	return nil
}

func printChangeInfoSection(title string, changes []ChangeInfo) {
	if len(changes) == 0 {
		return
	}
	fmt.Println(title)
	lastRepo := ""
	for _, change := range changes {
		if change.Repository != "" && change.Repository != lastRepo {
			fmt.Printf("  %s\n", change.Repository)
			lastRepo = change.Repository
		}
		symbol := getChangeSymbol(change.Type)
		indent := "  "
		if change.Repository != "" {
			indent = "    "
		}
		fmt.Printf("%s%s %s\n", indent, symbol, change.Path)
	}
}

func formatLocalCommitAuthor(commit LocalCommitInfo) string {
	name := strings.TrimSpace(commit.AuthorName)
	email := strings.TrimSpace(commit.AuthorEmail)
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	case email != "":
		return fmt.Sprintf("<%s>", email)
	default:
		return ""
	}
}

func localCommitState(commit LocalCommitInfo) string {
	if commit.Pushed {
		return "pushed"
	}
	return "pending"
}

func displayLogicalBranchName(branchName string) string {
	trimmed := strings.TrimSpace(branchName)
	if trimmed == "" {
		return "(default)"
	}
	return trimmed
}

func (sc *SessionCommand) showRefs(args []string) error {
	refsCmd := flag.NewFlagSet("refs", flag.ExitOnError)
	if err := refsCmd.Parse(args); err != nil {
		return err
	}

	resp, err := sc.sendCommand("refs")
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to get workspace refs: %s", resp.Error)
	}

	fmt.Printf("Workspace Refs\n")
	fmt.Printf("==============\n")
	fmt.Printf("Authority:  monofs-session is the source of truth; root git is synthetic\n\n")

	if len(resp.WorkspaceRefs) == 0 {
		fmt.Println("No included workspace repositories are currently visible.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BRANCH\tCOMMIT\tREPOSITORY")
	for _, ref := range resp.WorkspaceRefs {
		branch := firstNonEmpty(strings.TrimSpace(ref.Ref), "(unconfigured)")
		commit := shortCommitHash(ref.CommitHash)
		if commit == "" {
			commit = "(unknown)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", branch, commit, ref.DisplayPath)
	}
	return w.Flush()
}

func (sc *SessionCommand) manageBranches(args []string) error {
	op := "show"
	branchName := ""
	if len(args) > 0 {
		op = args[0]
		if len(args) > 1 {
			branchName = strings.TrimSpace(args[1])
		}
	}

	switch op {
	case "show":
		if len(args) > 1 {
			return fmt.Errorf("branch show does not take a branch name")
		}
		return sc.showLogicalBranches()
	case "create", "switch", "delete":
		if branchName == "" {
			return fmt.Errorf("branch %s requires a logical branch name", op)
		}
		if len(args) > 2 {
			return fmt.Errorf("branch %s takes exactly one logical branch name", op)
		}
		resp, err := sc.sendRequest(SessionRequest{Action: "branch", BranchOp: op, BranchName: branchName})
		if err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("failed to %s logical branch: %s", op, resp.Error)
		}
		fmt.Println(resp.Message)
		return nil
	default:
		return fmt.Errorf("unknown branch subcommand: %s", op)
	}
}

func (sc *SessionCommand) showLogicalBranches() error {
	resp, err := sc.sendRequest(SessionRequest{Action: "branch", BranchOp: "show"})
	if err != nil {
		return err
	}
	if !resp.Success {
		if resp.Error == "no active session" {
			fmt.Println("No active write session.")
			fmt.Println("\nUse 'monofs-session start' to begin a new session.")
			return nil
		}
		return fmt.Errorf("failed to get logical branch state: %s", resp.Error)
	}

	fmt.Printf("Logical Branches\n")
	fmt.Printf("================\n")
	fmt.Printf("Session ID: %s\n", resp.SessionID)
	fmt.Printf("Created:    %s\n", resp.CreatedAt)
	fmt.Printf("Current:    %s\n", displayLogicalBranchName(resp.CurrentBranch))
	fmt.Printf("Authority:  monofs-session is the source of truth; switching branches does not rewrite the working tree\n")

	if len(resp.BranchList) == 0 {
		fmt.Println()
		fmt.Println("No named logical branches yet.")
	} else {
		fmt.Println()
		fmt.Println("Known Branches:")
		for _, branch := range resp.BranchList {
			marker := " "
			if branch.Current {
				marker = "*"
			}
			line := fmt.Sprintf("%s %s", marker, displayLogicalBranchName(branch.Name))
			if branch.PendingCommits > 0 {
				line += fmt.Sprintf("  (%d pending local commit(s))", branch.PendingCommits)
			}
			if branch.HasMappings {
				line += "  [repo mappings tracked]"
			}
			fmt.Println(line)
		}
	}

	if len(resp.BranchMappings) > 0 {
		fmt.Println()
		fmt.Println("Current Branch Repo Mappings:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACTUAL\tTRACKED\tLAST PUSH\tREPOSITORY")
		for _, mapping := range resp.BranchMappings {
			lastPush := shortCommitHash(mapping.LastPushedCommit)
			if lastPush == "" {
				lastPush = "-"
			}
			tracked := firstNonEmpty(strings.TrimSpace(mapping.OriginalBranch), "-")
			actual := firstNonEmpty(strings.TrimSpace(mapping.ActualBranch), "-")
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", actual, tracked, lastPush, mapping.DisplayPath)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func (sc *SessionCommand) commitSession(args []string) error {
	commitCmd := flag.NewFlagSet("commit", flag.ExitOnError)
	message := commitCmd.String("m", "", "Logical commit message for the local virtual commit")
	messageLong := commitCmd.String("message", "", "Logical commit message for the local virtual commit")
	authorName := commitCmd.String("author-name", firstNonEmpty(os.Getenv("MONOFS_AUTHOR_NAME"), os.Getenv("GIT_AUTHOR_NAME"), os.Getenv("GIT_COMMITTER_NAME")), "Author name recorded on the local virtual commit")
	authorEmail := commitCmd.String("author-email", firstNonEmpty(os.Getenv("MONOFS_AUTHOR_EMAIL"), os.Getenv("GIT_AUTHOR_EMAIL"), os.Getenv("GIT_COMMITTER_EMAIL")), "Author email recorded on the local virtual commit")
	branchStrategy := commitCmd.String("branch-strategy", "direct", "Reserved for later push routing; ignored when creating local commits")
	pushMode := commitCmd.String("push-mode", "", "push mode used by a later push: squash or preserve (default: router configuration)")

	if err := commitCmd.Parse(args); err != nil {
		return err
	}
	if *branchStrategy != "direct" && *branchStrategy != "workspace_branch" && *branchStrategy != "per_repo_branch" {
		return fmt.Errorf("unsupported branch strategy %q", *branchStrategy)
	}
	if err := validatePushMode(*pushMode); err != nil {
		return err
	}

	finalMessage := firstNonEmpty(*messageLong, *message)

	fmt.Println("Creating local commit...")

	resp, err := sc.sendRequest(SessionRequest{
		Action:                  "commit",
		LogicalCommitMessage:    finalMessage,
		AuthorName:              *authorName,
		AuthorEmail:             *authorEmail,
		RequestedBranchStrategy: *branchStrategy,
		SourcePushMode:          normalizePushMode(*pushMode),
	})
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("commit failed: %s", resp.Error)
	}

	fmt.Printf("✓ Local commit created\n")
	fmt.Printf("  %s\n", resp.Message)

	return nil
}

func (sc *SessionCommand) pushSource(args []string) error {
	pushCmd := flag.NewFlagSet("push", flag.ExitOnError)
	mode := pushCmd.String("push-mode", "", "push mode: squash or preserve (default: mode set at commit time, else router configuration)")
	if err := pushCmd.Parse(args); err != nil {
		return err
	}
	if pushCmd.NArg() > 0 {
		return fmt.Errorf("push does not accept positional arguments")
	}
	if err := validatePushMode(*mode); err != nil {
		return err
	}

	fmt.Println("Pushing local commits...")

	resp, err := sc.sendRequest(SessionRequest{
		Action:         "push",
		SourcePushMode: normalizePushMode(*mode),
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("push failed: %s", resp.Error)
	}

	fmt.Printf("✓ Local commits pushed\n")
	if resp.Message != "" {
		fmt.Printf("  %s\n", resp.Message)
	}

	return nil
}

func (sc *SessionCommand) pullWorkspace() error {
	fmt.Println("Refreshing workspace...")

	resp, err := sc.sendCommand("pull")
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("pull failed: %s", resp.Error)
	}

	fmt.Printf("✓ Workspace refreshed\n")
	if resp.Message != "" {
		fmt.Printf("  %s\n", resp.Message)
	}

	return nil
}

func (sc *SessionCommand) showConflicts() error {
	resp, err := sc.sendCommand("conflicts")
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("conflicts failed: %s", resp.Error)
	}

	if len(resp.ConflictList) == 0 {
		fmt.Println("No unresolved conflicts")
		return nil
	}

	fmt.Printf("Unresolved conflicts (%d):\n", len(resp.ConflictList))
	for _, conflict := range resp.ConflictList {
		fmt.Printf("  %-60s %s\n", conflict.Path, conflict.Reason)
		if conflict.CreatedAt != "" {
			fmt.Printf("    conflicted at %s\n", conflict.CreatedAt)
		}
	}
	fmt.Println("\nResolve by editing each file (remove <<<<<<< ... >>>>>>> markers),")
	fmt.Println("then: monofs-session add <path>")

	return nil
}

func (sc *SessionCommand) discardSession() error {
	fmt.Println("Discarding session...")

	resp, err := sc.sendCommand("discard")
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("discard failed: %s", resp.Error)
	}

	fmt.Printf("✓ Session discarded\n")
	fmt.Println("All local changes have been removed.")

	return nil
}

func getChangeSymbol(changeType string) string {
	switch changeType {
	case "create":
		return "[+]"
	case "modify":
		return "[M]"
	case "delete":
		return "[-]"
	case "mkdir":
		return "[D+]"
	case "rmdir":
		return "[D-]"
	case "symlink":
		return "[L]"
	case "user_root_dir":
		return "[U+]"
	case "remove_user_root_dir":
		return "[U-]"
	default:
		return "[?]"
	}
}

func shortCommitHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// showBlobsInfo displays blob file information for the current session.
func (sc *SessionCommand) showBlobsInfo(args []string) error {
	depsCmd := flag.NewFlagSet("blobs-info", flag.ExitOnError)
	format := depsCmd.String("format", "table", "Output format: table or json")
	detailed := depsCmd.Bool("detailed", false, "Show individual file listings")

	if err := depsCmd.Parse(args); err != nil {
		return err
	}

	resp, err := sc.sendCommand("blobs-info")
	if err != nil {
		return err
	}

	if !resp.Success {
		if resp.Error == "no active session" {
			fmt.Println("No active write session.")
			return nil
		}
		return fmt.Errorf("blobs-info failed: %s", resp.Error)
	}

	if resp.DepsInfo == nil || resp.DepsInfo.TotalFiles == 0 {
		fmt.Println("No blob files in current session.")
		return nil
	}

	info := resp.DepsInfo

	if *format == "json" {
		data, _ := json.MarshalIndent(info, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	// Table format
	fmt.Printf("Dependency Files\n")
	fmt.Printf("================\n")
	fmt.Printf("Session:     %s\n", resp.SessionID)
	fmt.Printf("Total Files: %d\n", info.TotalFiles)
	fmt.Printf("Total Size:  %s\n", formatBytes(info.TotalBytes))
	fmt.Println()

	for _, tool := range info.Tools {
		fmt.Printf("  %-10s  %4d files  %s\n", tool.Tool, tool.Files, formatBytes(tool.Bytes))
		if *detailed && len(tool.FileList) > 0 {
			for _, f := range tool.FileList {
				fmt.Printf("    %-60s %s\n", f.Path, formatBytes(f.Size))
			}
		}
	}

	return nil
}

// formatBytes returns a human-readable byte size.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// uploadBlobs sends a dependency upload request to the FUSE client via socket.
// The actual work (ZIP packaging + upload) happens server-side.
func (sc *SessionCommand) uploadBlobs(args []string) error {
	uploadCmd := flag.NewFlagSet("push-blobs", flag.ExitOnError)

	if err := uploadCmd.Parse(args); err != nil {
		return err
	}

	fmt.Println("Uploading blob files...")

	resp, err := sc.sendCommand("push-blobs")
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("push failed: %s", resp.Error)
	}

	fmt.Printf("✓ Dependencies uploaded\n")
	if resp.Message != "" {
		fmt.Printf("  %s\n", resp.Message)
	}
	if resp.Changes > 0 {
		fmt.Printf("  Files: %d\n", resp.Changes)
	}

	return nil
}

// setupBlobs creates blob cache directories on the monofs filesystem
// and prints environment variable exports so all package managers store
// their caches on the shared filesystem.
func (sc *SessionCommand) setupBlobs(args []string) error {
	setupCmd := flag.NewFlagSet("setup", flag.ExitOnError)
	mountPath := setupCmd.String("mount", "", "MonoFS mount point (required)")
	shell := setupCmd.String("shell", "posix", "Shell format: posix, fish, json")
	tools := setupCmd.String("tools", "all", "Comma-separated list of tools: go,npm,pip,bazel,cargo (default: all)")

	if err := setupCmd.Parse(args); err != nil {
		return err
	}

	if *mountPath == "" {
		// Try MONOFS_MOUNT env
		if env := os.Getenv("MONOFS_MOUNT"); env != "" {
			*mountPath = env
		} else {
			setupCmd.Usage()
			return fmt.Errorf("--mount is required (or set MONOFS_MOUNT)")
		}
	}

	// Verify mount point exists
	if fi, err := os.Stat(*mountPath); err != nil || !fi.IsDir() {
		return fmt.Errorf("mount point %s does not exist or is not a directory", *mountPath)
	}

	// Determine which tools to configure
	enabled := map[string]bool{
		"go": true, "npm": true, "pip": true, "bazel": true, "cargo": true,
	}
	if *tools != "all" {
		enabled = map[string]bool{}
		for _, t := range strings.Split(*tools, ",") {
			t = strings.TrimSpace(strings.ToLower(t))
			if t != "" {
				enabled[t] = true
			}
		}
	}

	depsBase := filepath.Join(*mountPath, "dependency")

	var entries []setupEnvEntry

	if enabled["go"] {
		entries = append(entries,
			setupEnvEntry{"GOMODCACHE", filepath.Join(depsBase, "go", "mod")},
			setupEnvEntry{"GOPATH", filepath.Join(depsBase, "go", "path")},
		)
	}
	if enabled["npm"] {
		entries = append(entries,
			setupEnvEntry{"npm_config_cache", filepath.Join(depsBase, "npm", "cache")},
			setupEnvEntry{"NPM_CONFIG_PREFIX", filepath.Join(depsBase, "npm", "global")},
			setupEnvEntry{"YARN_CACHE_FOLDER", filepath.Join(depsBase, "npm", "yarn-cache")},
			setupEnvEntry{"PNPM_HOME", filepath.Join(depsBase, "npm", "pnpm")},
		)
	}
	if enabled["pip"] {
		entries = append(entries,
			setupEnvEntry{"PIP_CACHE_DIR", filepath.Join(depsBase, "pip", "cache")},
			setupEnvEntry{"PYTHONUSERBASE", filepath.Join(depsBase, "pip", "user")},
		)
	}
	if enabled["bazel"] {
		entries = append(entries,
			setupEnvEntry{"BAZEL_REPOSITORY_CACHE", filepath.Join(depsBase, "bazel", "repo-cache")},
			setupEnvEntry{"BAZEL_OUTPUT_BASE", filepath.Join(depsBase, "bazel", "output-base")},
		)
	}
	if enabled["cargo"] {
		entries = append(entries,
			setupEnvEntry{"CARGO_HOME", filepath.Join(depsBase, "cargo")},
		)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no valid tools specified; choose from: go, npm, pip, bazel, cargo")
	}

	var created int
	for _, e := range entries {
		if _, err := os.Stat(e.dir); err == nil {
			continue // already exists
		}
		if err := os.MkdirAll(e.dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", e.dir, err)
		}
		created++
	}

	// Output environment variables in the requested format
	switch *shell {
	case "posix":
		for _, e := range entries {
			fmt.Printf("export %s=%q\n", e.envVar, e.dir)
		}
	case "fish":
		for _, e := range entries {
			fmt.Printf("set -gx %s %q;\n", e.envVar, e.dir)
		}
	case "json":
		m := make(map[string]string, len(entries))
		for _, e := range entries {
			m[e.envVar] = e.dir
		}
		data, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(data))
	default:
		return fmt.Errorf("unknown shell format %q; use posix, fish, or json", *shell)
	}

	// Print summary to stderr so eval doesn't capture it
	if created > 0 {
		fmt.Fprintf(os.Stderr, "monofs-session: created %d cache dirs under %s\n", created, depsBase)
	} else {
		fmt.Fprintf(os.Stderr, "monofs-session: %d cache dirs ready under %s\n", len(entries), depsBase)
	}

	return nil
}

// searchCode performs code search using the MonoFSSearch service
func (sc *SessionCommand) searchCode(args []string) error {
	// Parse search flags
	searchCmd := flag.NewFlagSet("search", flag.ExitOnError)
	query := searchCmd.String("query", "", "Search query (required)")
	searchAddr := searchCmd.String("search", "localhost:9100", "MonoFS search service address")
	storageID := searchCmd.String("storage-id", "", "Limit search to specific repository")
	maxResults := searchCmd.Int("max-results", 50, "Maximum number of results")
	caseSensitive := searchCmd.Bool("case-sensitive", false, "Case-sensitive search")
	regex := searchCmd.Bool("regex", false, "Treat query as regular expression")
	filePattern := searchCmd.String("file-pattern", "", "File glob pattern (e.g., *.go)")
	symbol := searchCmd.String("symbol", "", "Search for a symbol (maps to Zoekt sym: query, e.g. a function/type name)")

	if err := searchCmd.Parse(args); err != nil {
		return err
	}

	if *query == "" && *symbol == "" {
		searchCmd.Usage()
		return fmt.Errorf("--query or --symbol is required")
	}

	effectiveQuery := *query
	if *symbol != "" {
		effectiveQuery = "sym:" + *symbol
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Connect to search service
	conn, err := grpc.DialContext(ctx, *searchAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to search service at %s: %w", *searchAddr, err)
	}
	defer conn.Close()

	client := pb.NewMonoFSSearchClient(conn)

	// Build search request
	req := &pb.SearchRequest{
		Query:         effectiveQuery,
		MaxResults:    int32(*maxResults),
		CaseSensitive: *caseSensitive,
		Regex:         *regex,
	}

	if *storageID != "" {
		req.StorageId = *storageID
	}

	if *filePattern != "" {
		req.FilePatterns = []string{*filePattern}
	}

	// Execute search
	resp, err := client.Search(ctx, req)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	// Display results
	return displaySearchResults(resp, effectiveQuery)
}

// displaySearchResults formats and displays search results
func displaySearchResults(resp *pb.SearchResponse, query string) error {
	if len(resp.Results) == 0 {
		fmt.Printf("No results found for: %s\n", query)
		return nil
	}

	fmt.Printf("Found %d results (searched %d files in %dms)\n\n",
		resp.TotalMatches, resp.FilesSearched, resp.DurationMs)

	if resp.Truncated {
		fmt.Println("⚠ Results truncated. Use --max-results to see more.")
		fmt.Println()
	}

	for i, result := range resp.Results {
		// Display repository and file path
		fmt.Printf("[%d] %s:%s:%d\n",
			i+1,
			result.DisplayPath,
			result.FilePath,
			result.LineNumber,
		)

		// Display context if available
		if result.BeforeContext != "" {
			fmt.Printf("    %s\n", colorize(result.BeforeContext, "", false))
		}

		// Display matched line with highlighting
		fmt.Printf("  > %s\n", highlightMatches(result.LineContent, result.Matches))

		if result.AfterContext != "" {
			fmt.Printf("    %s\n", colorize(result.AfterContext, "", false))
		}

		fmt.Println()
	}

	return nil
}

// highlightMatches highlights match ranges in the line content
func highlightMatches(line string, matches []*pb.MatchRange) string {
	if len(matches) == 0 {
		return line
	}

	lineLen := int32(len(line))

	// Build highlighted string
	var result strings.Builder
	lastEnd := int32(0)

	for _, match := range matches {
		start := match.Start
		end := match.End

		// Clamp to line bounds
		if start < 0 {
			start = 0
		}
		if end > lineLen {
			end = lineLen
		}
		if start >= lineLen || start >= end {
			continue
		}

		// Add text before match
		if start > lastEnd {
			result.WriteString(line[lastEnd:start])
		}

		// Add highlighted match (using ANSI codes for terminal)
		matchText := line[start:end]
		result.WriteString(colorize(matchText, "", true))

		lastEnd = end
	}

	// Add remaining text
	if lastEnd < lineLen {
		result.WriteString(line[lastEnd:])
	}

	return result.String()
}

// colorize adds ANSI color codes for terminal output
func colorize(text string, color string, bold bool) string {
	// If no color specified, default to bold yellow
	if color == "" {
		return fmt.Sprintf("\033[1;33m%s\033[0m", text)
	}

	// Map of common color names to ANSI color codes
	colors := map[string]int{
		"black":   30,
		"red":     31,
		"green":   32,
		"yellow":  33,
		"blue":    34,
		"magenta": 35,
		"cyan":    36,
		"white":   37,
	}

	code, ok := colors[strings.ToLower(color)]
	if !ok {
		// Unknown color: return unmodified
		return text
	}

	if bold {
		return fmt.Sprintf("\033[1;%dm%s\033[0m", code, text)
	}
	return fmt.Sprintf("\033[0;%dm%s\033[0m", code, text)
}
