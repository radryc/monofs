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

// SessionRequest is sent to the FUSE client
type SessionRequest struct {
	Action string `json:"action"` // start, status, commit, discard
}

// SessionResponse is received from the FUSE client
type SessionResponse struct {
	Success    bool         `json:"success"`
	SessionID  string       `json:"session_id,omitempty"`
	CreatedAt  string       `json:"created_at,omitempty"`
	Changes    int          `json:"changes,omitempty"`
	Message    string       `json:"message,omitempty"`
	Error      string       `json:"error,omitempty"`
	ChangeList []ChangeInfo `json:"change_list,omitempty"`
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
		return sc.showStatus()
	case "commit":
		return sc.commitSession()
	case "discard":
		return sc.discardSession()
	case "search":
		return sc.searchCode(args[1:])
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
  start    Start a new write session (or show current if active)
  status   Show current session status and pending changes
  commit   Push local changes to backend and archive session
  discard  Abandon all local changes and delete session
  search   Search code across indexed repositories
  help     Show this help message

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
	conn.SetDeadline(time.Now().Add(30 * time.Second))

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

func (sc *SessionCommand) showStatus() error {
	resp, err := sc.sendCommand("status")
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
	fmt.Printf("Changes:    %d\n", resp.Changes)
	fmt.Println()

	if len(resp.ChangeList) > 0 {
		fmt.Println("Pending Changes:")
		for _, change := range resp.ChangeList {
			symbol := getChangeSymbol(change.Type)
			fmt.Printf("  %s %s\n", symbol, change.Path)
		}
	} else {
		fmt.Println("No changes yet.")
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

// searchCode performs code search using the MonoFSSearch service
func (sc *SessionCommand) searchCode(args []string) error {
	// Parse search flags
	searchCmd := flag.NewFlagSet("search", flag.ExitOnError)
	query := searchCmd.String("query", "", "Search query (required)")
	searchAddr := searchCmd.String("search", "localhost:9091", "MonoFS search service address")
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

	// Build highlighted string
	var result strings.Builder
	lastEnd := 0

	for _, match := range matches {
		// Add text before match
		if match.Start > int32(lastEnd) {
			result.WriteString(line[lastEnd:match.Start])
		}

		// Add highlighted match (using ANSI codes for terminal)
		matchText := line[match.Start:match.End]
		result.WriteString(colorize(matchText, "", true))

		lastEnd = int(match.End)
	}

	// Add remaining text
	if lastEnd < len(line) {
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
