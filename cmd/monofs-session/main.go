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
	Action    string `json:"action"` // start, status, commit, discard, diff
	Path      string `json:"path,omitempty"`
	ShowBlobs bool   `json:"show_blobs,omitempty"`
}

// FileDiff holds the unified diff output for a single changed file.
type FileDiff struct {
	Path       string `json:"path"`
	ChangeType string `json:"change_type"`
	Diff       string `json:"diff"`
}

// SessionResponse is received from the FUSE client
type SessionResponse struct {
	Success        bool           `json:"success"`
	SessionID      string         `json:"session_id,omitempty"`
	CreatedAt      string         `json:"created_at,omitempty"`
	Changes        int            `json:"changes,omitempty"`
	BlobChanges    int            `json:"blob_changes,omitempty"`
	Message        string         `json:"message,omitempty"`
	Error          string         `json:"error,omitempty"`
	ChangeList     []ChangeInfo   `json:"change_list,omitempty"`
	BlobChangeList []ChangeInfo   `json:"blob_change_list,omitempty"`
	DepsInfo       *BlobsInfoData `json:"deps_info,omitempty"`
	DiffData       []FileDiff     `json:"diff_data,omitempty"`
	BlobDiffData   []FileDiff     `json:"blob_diff_data,omitempty"`
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

// ChangeInfo represents a single change for display
type ChangeInfo struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
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
		// Check for MONOFS_OVERLAY_DIR environment variable first
		if envDir := os.Getenv("MONOFS_OVERLAY_DIR"); envDir != "" {
			socketPath = filepath.Join(envDir, "session.sock")
		} else {
			// Default to ~/.monofs/overlay
			homeDir, _ := os.UserHomeDir()
			socketPath = filepath.Join(homeDir, ".monofs", "overlay", "session.sock")
		}
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
	case "status":
		return sc.showStatus(args[1:])
	case "commit":
		return sc.commitSession()
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
  start        Start a new write session (or show current if active)
  status       Show current session status and pending changes
  diff [file]  Show unified diff between original and changed files
  commit       Push local changes to backend and archive session
  discard      Abandon all local changes and delete session
  blobs-info    Show blob files in the current session
  push          Package and upload blob files to storage backend
  search       Search code across indexed repositories
  setup        Create blob cache dirs on monofs and print env exports
  help         Show this help message

Options:
  --socket <path>  Explicit path to session socket file

Write sessions allow you to make local modifications that are tracked
and can be committed to the Git backend when ready.

Environment:
  MONOFS_OVERLAY_DIR  Override default overlay location (~/.monofs/overlay)

Examples:
  # Start a new session
  monofs-session start

  # Check what changes are pending
  monofs-session status

  # Include blob file changes in status/diff
  monofs-session status --deps
  monofs-session diff --deps

  # Search for code
  monofs-session search --query "func main" --max-results 10

  # Search with filters
  monofs-session search --query "TODO" --regex --case-sensitive

  # Use explicit socket path (useful in Docker)
  monofs-session --socket /path/to/session.sock status

  # Commit all changes to backend
  monofs-session commit

  # Abandon all local changes
  monofs-session discard

  # Upload blob files to storage backend
  monofs-session push

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

  2. Set GITFS_OVERLAY_DIR to match monofs-client's --overlay path:
     export GITFS_OVERLAY_DIR=/path/to/overlay
     monofs-session status

  3. Use --socket to specify explicit path:
     monofs-session --socket /path/to/session.sock status

Common socket locations:
  - %s/.monofs/overlay/session.sock (default)
  - /tmp/monofs-overlay/session.sock (Docker common)

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
		return nil, fmt.Errorf("session socket not found at %s (is monofs-client running with --writable?)", sc.socketPath)
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
	case "push", "push-blobs":
		// Push keeps the socket open until upload, backend verification,
		// and overlay cleanup all finish.
		return pushSocketTimeout
	default:
		return defaultSocketTimeout
	}
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
	fmt.Printf("Use 'monofs-session commit' when ready to push changes.\n")

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
	fmt.Printf("Changes:    %d file(s)\n", resp.Changes)
	if resp.BlobChanges > 0 {
		if *showBlobs {
			fmt.Printf("Blobs:       %d file(s)  (use 'push' to upload)\n", resp.BlobChanges)
		} else {
			fmt.Printf("Blobs:       %d file(s)  (use --deps to show, 'push' to upload)\n", resp.BlobChanges)
		}
	}
	fmt.Println()

	if len(resp.ChangeList) > 0 {
		fmt.Println("Pending Changes:")
		for _, change := range resp.ChangeList {
			symbol := getChangeSymbol(change.Type)
			fmt.Printf("  %s %s\n", symbol, change.Path)
		}
	} else if resp.BlobChanges == 0 {
		fmt.Println("No changes yet.")
	}

	if *showBlobs && len(resp.BlobChangeList) > 0 {
		fmt.Println()
		fmt.Println("Dependency Changes:")
		for _, change := range resp.BlobChangeList {
			symbol := getChangeSymbol(change.Type)
			fmt.Printf("  %s %s\n", symbol, change.Path)
		}
	}

	return nil
}

func (sc *SessionCommand) commitSession() error {
	fmt.Println("Committing changes...")

	resp, err := sc.sendCommand("commit")
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("commit failed: %s", resp.Error)
	}

	fmt.Printf("✓ Session committed successfully\n")
	fmt.Printf("  %s\n", resp.Message)

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
	uploadCmd := flag.NewFlagSet("push", flag.ExitOnError)

	if err := uploadCmd.Parse(args); err != nil {
		return err
	}

	fmt.Println("Uploading blob files...")

	resp, err := sc.sendCommand("push")
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

	// Define directory layout and env vars per tool
	type envEntry struct {
		envVar string
		dir    string
	}

	var entries []envEntry

	if enabled["go"] {
		entries = append(entries,
			envEntry{"GOMODCACHE", filepath.Join(depsBase, "go", "mod")},
			envEntry{"GOPATH", filepath.Join(depsBase, "go", "path")},
		)
	}
	if enabled["npm"] {
		entries = append(entries,
			envEntry{"npm_config_cache", filepath.Join(depsBase, "npm", "cache")},
			envEntry{"NPM_CONFIG_PREFIX", filepath.Join(depsBase, "npm", "global")},
			envEntry{"YARN_CACHE_FOLDER", filepath.Join(depsBase, "npm", "yarn-cache")},
			envEntry{"PNPM_HOME", filepath.Join(depsBase, "npm", "pnpm")},
		)
	}
	if enabled["pip"] {
		entries = append(entries,
			envEntry{"PIP_CACHE_DIR", filepath.Join(depsBase, "pip", "cache")},
			envEntry{"PYTHONUSERBASE", filepath.Join(depsBase, "pip", "user")},
		)
	}
	if enabled["bazel"] {
		entries = append(entries,
			envEntry{"BAZEL_REPOSITORY_CACHE", filepath.Join(depsBase, "bazel", "repo-cache")},
			envEntry{"BAZEL_OUTPUT_BASE", filepath.Join(depsBase, "bazel", "output-base")},
		)
	}
	if enabled["cargo"] {
		entries = append(entries,
			envEntry{"CARGO_HOME", filepath.Join(depsBase, "cargo")},
		)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no valid tools specified; choose from: go, npm, pip, bazel, cargo")
	}

	// Create directories that don't already exist
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

	if err := searchCmd.Parse(args); err != nil {
		return err
	}

	if *query == "" {
		searchCmd.Usage()
		return fmt.Errorf("--query is required")
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
		Query:         *query,
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
	return displaySearchResults(resp, *query)
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
