package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/buildlayout"
	golangmapper "github.com/radryc/monofs/internal/buildlayout/golang"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

func main() {
	// Subcommands
	ingestCmd := flag.NewFlagSet("ingest", flag.ExitOnError)
	ingestDepsCmd := flag.NewFlagSet("ingest-deps", flag.ExitOnError)
	statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
	deleteCmd := flag.NewFlagSet("delete", flag.ExitOnError)
	failoverCmd := flag.NewFlagSet("failover", flag.ExitOnError)
	rebuildIndexCmd := flag.NewFlagSet("rebuild-index", flag.ExitOnError)
	reposCmd := flag.NewFlagSet("repos", flag.ExitOnError)
	rebalanceCmd := flag.NewFlagSet("rebalance", flag.ExitOnError)
	statsCmd := flag.NewFlagSet("stats", flag.ExitOnError)
	drainCmd := flag.NewFlagSet("drain", flag.ExitOnError)
	undrainCmd := flag.NewFlagSet("undrain", flag.ExitOnError)
	triggerFailoverCmd := flag.NewFlagSet("trigger-failover", flag.ExitOnError)
	clearFailoverCmd := flag.NewFlagSet("clear-failover", flag.ExitOnError)
	nodeFilesCmd := flag.NewFlagSet("node-files", flag.ExitOnError)
	fetchersCmd := flag.NewFlagSet("fetchers", flag.ExitOnError)

	// Ingest flags
	routerAddr := ingestCmd.String("router", "localhost:9090", "MonoFS router address")
	source := ingestCmd.String("source", "", "Source: Git URL, Go module path (module@version), S3 bucket (required)")
	ref := ingestCmd.String("ref", "", "Ref: branch for Git, version for Go, prefix for S3 (optional)")
	sourceID := ingestCmd.String("source-id", "", "Custom source ID (optional, auto-generated if empty)")
	ingestionType := ingestCmd.String("ingestion-type", "git", "Ingestion backend type (git, go, s3, file)")
	fetchType := ingestCmd.String("fetch-type", "git", "Fetch backend type (git, gomod, s3, local)")
	replicateData := ingestCmd.Bool("replicate-data", false, "Replicate blob data to fetch backend during ingestion")

	// Ingest-deps flags
	ingestDepsRouter := ingestDepsCmd.String("router", "localhost:9090", "MonoFS router address")
	ingestDepsFile := ingestDepsCmd.String("file", "", "Path to dependency manifest (e.g., go.mod, package.json) (required)")
	ingestDepsType := ingestDepsCmd.String("type", "go", "Dependency type: go, npm, maven, cargo")
	ingestDepsConcurrency := ingestDepsCmd.Int("concurrency", 5, "Max concurrent ingestions")
	ingestDepsSkipExisting := ingestDepsCmd.Bool("skip-existing", true, "Skip dependencies that are already ingested")

	// Delete flags
	deleteRouter := deleteCmd.String("router", "localhost:9090", "MonoFS router address")
	deleteStorageID := deleteCmd.String("storage-id", "", "Storage ID to delete (required)")

	// Status flags
	statusRouter := statusCmd.String("router", "localhost:9090", "MonoFS router address")

	// NEW: Failover flags
	failoverRouter := failoverCmd.String("router", "localhost:9090", "MonoFS router address")

	// Rebuild index flags
	rebuildRouter := rebuildIndexCmd.String("router", "localhost:9090", "MonoFS router address")
	rebuildStorageID := rebuildIndexCmd.String("storage-id", "", "Storage ID to rebuild (required)")
	rebuildExternal := rebuildIndexCmd.Bool("external", true, "Use external node addresses (localhost:9001-9005)")
	rebuildDebug := rebuildIndexCmd.Bool("debug", false, "Enable debug logging")

	// Repos flags
	reposRouter := reposCmd.String("router", "localhost:8080", "MonoFS router HTTP address")

	// Rebalance flags
	rebalanceRouter := rebalanceCmd.String("router", "localhost:8080", "MonoFS router HTTP address")
	rebalanceStorageID := rebalanceCmd.String("storage-id", "", "Storage ID to rebalance (required)")

	// Stats flags
	statsRouter := statsCmd.String("router", "localhost:9090", "MonoFS router gRPC address")
	statsType := statsCmd.String("type", "cluster", "Statistics type: cluster, nodes, or all")
	statsFormat := statsCmd.String("format", "table", "Output format: table or json")

	// Drain flags
	drainRouter := drainCmd.String("router", "localhost:9090", "MonoFS router gRPC address")
	drainReason := drainCmd.String("reason", "planned maintenance", "Reason for draining cluster")

	// Undrain flags
	undrainRouter := undrainCmd.String("router", "localhost:9090", "MonoFS router gRPC address")

	// Trigger failover flags
	triggerFailoverRouter := triggerFailoverCmd.String("router", "localhost:9090", "MonoFS router gRPC address")
	triggerFailoverNodeID := triggerFailoverCmd.String("node-id", "", "Node ID to trigger failover for (required)")

	// Clear failover flags
	clearFailoverRouter := clearFailoverCmd.String("router", "localhost:9090", "MonoFS router gRPC address")
	clearFailoverNodeID := clearFailoverCmd.String("node-id", "", "Node ID to clear failover for (required)")

	// Node files flags
	nodeFilesRouter := nodeFilesCmd.String("router", "localhost:9090", "MonoFS router gRPC address")
	nodeFilesNodeID := nodeFilesCmd.String("node-id", "", "Node ID to list files for (required)")
	nodeFilesStorageID := nodeFilesCmd.String("storage-id", "", "Storage ID to filter by (optional)")
	nodeFilesFormat := nodeFilesCmd.String("format", "table", "Output format: table or json")

	// Fetchers flags
	fetchersRouter := fetchersCmd.String("router", "localhost:8080", "MonoFS router HTTP address")
	fetchersFormat := fetchersCmd.String("format", "table", "Output format: table or json")
	fetchersDetailed := fetchersCmd.Bool("detailed", false, "Show per-source statistics")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ingest":
		ingestCmd.Parse(os.Args[2:])
		if *source == "" {
			fmt.Fprintln(os.Stderr, "Error: --source is required")
			ingestCmd.Usage()
			os.Exit(1)
		}

		if err := ingestRepository(*routerAddr, *source, *ref, *sourceID, *ingestionType, *fetchType, *replicateData); err != nil {
			fmt.Fprintf(os.Stderr, "Error: ingestion failed: %v\n", err)
			os.Exit(1)
		}

	case "ingest-deps":
		ingestDepsCmd.Parse(os.Args[2:])
		if *ingestDepsFile == "" {
			fmt.Fprintln(os.Stderr, "Error: --file is required")
			ingestDepsCmd.Usage()
			os.Exit(1)
		}

		if err := ingestDeps(*ingestDepsRouter, *ingestDepsFile, *ingestDepsType, *ingestDepsConcurrency, *ingestDepsSkipExisting); err != nil {
			fmt.Fprintf(os.Stderr, "Error: dependency ingestion failed: %v\n", err)
			os.Exit(1)
		}

	case "delete":
		deleteCmd.Parse(os.Args[2:])
		if *deleteStorageID == "" {
			fmt.Fprintln(os.Stderr, "Error: --storage-id is required")
			deleteCmd.Usage()
			os.Exit(1)
		}

		if err := deleteRepository(*deleteRouter, *deleteStorageID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: deletion failed: %v\n", err)
			os.Exit(1)
		}

	case "status":
		statusCmd.Parse(os.Args[2:])

		if err := showClusterStatus(*statusRouter); err != nil {
			fmt.Fprintf(os.Stderr, "Error: status check failed: %v\n", err)
			os.Exit(1)
		}

	case "failover": // NEW
		failoverCmd.Parse(os.Args[2:])

		if err := showFailoverStatus(*failoverRouter); err != nil {
			fmt.Fprintf(os.Stderr, "Error: failover status check failed: %v\n", err)
			os.Exit(1)
		}

	case "rebuild-index":
		rebuildIndexCmd.Parse(os.Args[2:])

		if *rebuildStorageID == "" {
			fmt.Fprintln(os.Stderr, "Error: --storage-id is required")
			rebuildIndexCmd.Usage()
			os.Exit(1)
		}

		if err := rebuildDirectoryIndex(*rebuildRouter, *rebuildStorageID, *rebuildExternal, *rebuildDebug); err != nil {
			fmt.Fprintf(os.Stderr, "Error: rebuild index failed: %v\n", err)
			os.Exit(1)
		}

	case "repos":
		reposCmd.Parse(os.Args[2:])

		if err := showRepositories(*reposRouter); err != nil {
			fmt.Fprintf(os.Stderr, "Error: repos check failed: %v\n", err)
			os.Exit(1)
		}

	case "rebalance":
		rebalanceCmd.Parse(os.Args[2:])

		if *rebalanceStorageID == "" {
			fmt.Fprintln(os.Stderr, "Error: --storage-id is required")
			rebalanceCmd.Usage()
			os.Exit(1)
		}

		if err := triggerRebalance(*rebalanceRouter, *rebalanceStorageID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: rebalance failed: %v\n", err)
			os.Exit(1)
		}

	case "stats":
		statsCmd.Parse(os.Args[2:])

		if err := showStatistics(*statsRouter, *statsType, *statsFormat); err != nil {
			fmt.Fprintf(os.Stderr, "Error: stats failed: %v\n", err)
			os.Exit(1)
		}

	case "drain":
		drainCmd.Parse(os.Args[2:])

		if err := drainCluster(*drainRouter, *drainReason); err != nil {
			fmt.Fprintf(os.Stderr, "Error: drain failed: %v\n", err)
			os.Exit(1)
		}

	case "undrain":
		undrainCmd.Parse(os.Args[2:])

		if err := undrainCluster(*undrainRouter); err != nil {
			fmt.Fprintf(os.Stderr, "Error: undrain failed: %v\n", err)
			os.Exit(1)
		}

	case "trigger-failover":
		triggerFailoverCmd.Parse(os.Args[2:])

		if *triggerFailoverNodeID == "" {
			fmt.Fprintln(os.Stderr, "Error: --node-id is required")
			triggerFailoverCmd.Usage()
			os.Exit(1)
		}

		if err := triggerFailover(*triggerFailoverRouter, *triggerFailoverNodeID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: trigger-failover failed: %v\n", err)
			os.Exit(1)
		}

	case "clear-failover":
		clearFailoverCmd.Parse(os.Args[2:])

		if *clearFailoverNodeID == "" {
			fmt.Fprintln(os.Stderr, "Error: --node-id is required")
			clearFailoverCmd.Usage()
			os.Exit(1)
		}

		if err := clearFailover(*clearFailoverRouter, *clearFailoverNodeID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: clear-failover failed: %v\n", err)
			os.Exit(1)
		}

	case "node-files":
		nodeFilesCmd.Parse(os.Args[2:])

		if *nodeFilesNodeID == "" {
			fmt.Fprintln(os.Stderr, "Error: --node-id is required")
			nodeFilesCmd.Usage()
			os.Exit(1)
		}

		if err := showNodeFiles(*nodeFilesRouter, *nodeFilesNodeID, *nodeFilesStorageID, *nodeFilesFormat); err != nil {
			fmt.Fprintf(os.Stderr, "Error: node-files failed: %v\n", err)
			os.Exit(1)
		}

	case "fetchers":
		fetchersCmd.Parse(os.Args[2:])

		if err := showFetchers(*fetchersRouter, *fetchersFormat, *fetchersDetailed); err != nil {
			fmt.Fprintf(os.Stderr, "Error: fetchers failed: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `MonoFS Admin - Administration tool for MonoFS

Usage:
  monofs-admin <command> [options]

Commands:
  ingest           Ingest a Git repository or Go module into the cluster
  ingest-deps      Bulk ingest all dependencies from a manifest (go.mod, package.json, etc.)
  delete           Delete a repository from all nodes (cleanup)
  status           Show cluster status and health
  repos            Show repositories in the cluster
  rebalance        Trigger rebalancing for a specific repository
  failover         Show failover status and node backup mappings
  rebuild-index    Rebuild directory indexes for a repository
  stats            Show cluster or node statistics
  drain            Put cluster in maintenance mode (disable failover)
  undrain          Exit maintenance mode (enable failover)
  trigger-failover Manually trigger graceful failover for a node
  clear-failover   Clear failover state for a recovered node
  node-files       List files owned by a specific node
  fetchers         Show fetcher cluster status and statistics

Examples:
  # Ingest repository (auto-detect branch from URL)
  monofs-admin ingest --url=https://github.com/radryc/prompusher/tree/main
  
  # Ingest Go module
  monofs-admin ingest --source=github.com/google/uuid@v1.6.0 --ingestion-type=go --fetch-type=gomod

  # Bulk ingest all dependencies from go.mod
  monofs-admin ingest-deps --file=go.mod --type=go --concurrency=10

  # Delete repository to free memory
  monofs-admin delete --storage-id=<storage-id>

  # Ingest with custom repo ID
  monofs-admin ingest --url=https://github.com/owner/repo/tree/develop --repo-id=myrepo

  # Show cluster status
  monofs-admin status --router=localhost:9090
  
  # Show repositories
  monofs-admin repos --router=localhost:8080

  # Trigger rebalancing for a repository
  monofs-admin rebalance --router=localhost:8080 --storage-id=<hash>
  
  # Show failover status
  monofs-admin failover --router=localhost:9090

  # Show cluster statistics
  monofs-admin stats --type=cluster --format=table

  # Show node statistics in JSON format
  monofs-admin stats --type=nodes --format=json

  # Show all statistics
  monofs-admin stats --type=all

  # Rebuild directory indexes
  monofs-admin rebuild-index --router=localhost:9090 --storage-id=<hash>

  # Show fetcher cluster status
  monofs-admin fetchers --router=localhost:8080 --format=table

  # Show detailed fetcher stats with per-source breakdown
  monofs-admin fetchers --router=localhost:8080 --detailed

  # Trigger graceful failover for a node
  monofs-admin trigger-failover --router=localhost:9090 --node-id=node-1

  # Clear failover state after node recovery
  monofs-admin clear-failover --router=localhost:9090 --node-id=node-1

  # List files stored on a specific node (for debugging sharding)
  monofs-admin node-files --router=localhost:9090 --node-id=node1

  # List files for a specific repository on a node (shows only files on THAT node)
  monofs-admin node-files --router=localhost:9090 --node-id=node1 --storage-id=<hash>

  # List ALL files for a repository across ALL nodes
  monofs-admin node-files --router=localhost:9090 --node-id=all --storage-id=<hash>

Ingest Options:
  --router     Router gRPC address (default: localhost:9090)
  --url        Git repository URL (required, supports GitHub /tree/branch format)
  --repo-id    Custom repository ID (optional, auto-generated from URL if empty)
  --debug      Enable debug logging

Status Options:
  --router     Router gRPC address (default: localhost:9090)
  --debug      Enable debug logging

Failover Options:
  --router     Router gRPC address (default: localhost:9090)
  --debug      Enable debug logging

Rebuild Index Options:
  --router      Router gRPC address (default: localhost:9090)
  --storage-id  Storage ID of repository to rebuild (required)
  --debug       Enable debug logging

Trigger Failover Options:
  --router      Router gRPC address (default: localhost:9090)
  --node-id     Node ID to trigger failover for (required)

Clear Failover Options:
  --router      Router gRPC address (default: localhost:9090)
  --node-id     Node ID to clear failover for (required)

Node Files Options:
  --router      Router gRPC address (default: localhost:9090)
  --node-id     Node ID to list files for (required)
  --storage-id  Storage ID to filter by (optional)
  --format      Output format: table or json (default: table)
`)
}

// parseGitHubURL extracts repository URL and branch from GitHub URLs.
// Supports formats:
//   - https://github.com/owner/repo
//   - https://github.com/owner/repo/tree/branch
//   - https://github.com/owner/repo.git
func parseGitHubURL(rawURL string) (repoURL, branch string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}

	// Split path into segments
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")

	// Minimum: owner/repo
	if len(segments) < 2 {
		return "", "", fmt.Errorf("invalid GitHub URL format: expected owner/repo")
	}

	owner := segments[0]
	repo := strings.TrimSuffix(segments[1], ".git")

	// Check for /tree/branch pattern
	if len(segments) >= 4 && segments[2] == "tree" {
		branch = strings.Join(segments[3:], "/") // Support branches with slashes
		repoURL = fmt.Sprintf("%s://%s/%s/%s.git", u.Scheme, u.Host, owner, repo)
		return repoURL, branch, nil
	}

	// No branch specified, default to main
	repoURL = fmt.Sprintf("%s://%s/%s/%s.git", u.Scheme, u.Host, owner, repo)
	return repoURL, "main", nil
}

// validateIngestionParams validates source and ref based on ingestion type
func validateIngestionParams(source, ref, ingestionType string) error {
	switch ingestionType {
	case "git":
		if source == "" {
			return fmt.Errorf("--source (repository URL) is required for Git ingestion")
		}
		if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") && !strings.HasPrefix(source, "git@") {
			return fmt.Errorf("invalid Git URL format (must start with http://, https://, or git@)")
		}

	case "go":
		if source == "" {
			return fmt.Errorf("--source (module path) is required for Go module ingestion")
		}
		// Check for module@version format
		if strings.Contains(source, "@") {
			parts := strings.Split(source, "@")
			if len(parts) != 2 {
				return fmt.Errorf("invalid Go module format, expected module@version")
			}
			if !strings.HasPrefix(parts[1], "v") {
				return fmt.Errorf("Go module version must start with 'v' (e.g., v1.0.0)")
			}
		} else if ref == "" {
			return fmt.Errorf("Go module requires version either in source (module@version) or via --ref flag")
		} else if !strings.HasPrefix(ref, "v") {
			return fmt.Errorf("Go module version must start with 'v' (e.g., v1.0.0)")
		}
		if !strings.Contains(source, "/") {
			return fmt.Errorf("Go module path must contain at least one slash")
		}

	case "s3":
		if source == "" {
			return fmt.Errorf("--source (bucket name) is required for S3 ingestion")
		}
	}

	return nil
}

// ingestDeps reads a dependency manifest and ingests all dependencies.
func ingestDeps(routerAddr, filePath, depType string, concurrency int, skipExisting bool) error {
	// 1. Read the dependency file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read dependency file: %w", err)
	}

	// 2. Select mapper by type
	mapper := selectMapper(depType)
	if mapper == nil {
		return fmt.Errorf("unknown dependency type: %q (supported: go)", depType)
	}

	// 3. Parse dependencies
	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		return fmt.Errorf("parse dependencies: %w", err)
	}

	if len(deps) == 0 {
		fmt.Println("No dependencies found.")
		return nil
	}

	fmt.Printf("Found %d dependencies in %s\n", len(deps), filePath)

	// 4. Get existing repos if skip-existing is enabled
	existingRepos := make(map[string]bool)
	if skipExisting {
		repos, err := fetchExistingRepos(routerAddr)
		if err != nil {
			fmt.Printf("Warning: could not fetch existing repos: %v\n", err)
		} else {
			for _, repo := range repos {
				existingRepos[repo] = true
			}
			fmt.Printf("Found %d existing repositories\n", len(existingRepos))
		}
	}

	// 5. Filter dependencies
	var depsToIngest []buildlayout.Dependency
	for _, dep := range deps {
		if skipExisting && existingRepos[dep.Source] {
			fmt.Printf("Skipping %s (already ingested)\n", dep.Source)
			continue
		}
		depsToIngest = append(depsToIngest, dep)
	}

	if len(depsToIngest) == 0 {
		fmt.Println("All dependencies already ingested.")
		return nil
	}

	fmt.Printf("Ingesting %d dependencies with concurrency %d...\n", len(depsToIngest), concurrency)

	// 6. Ingest dependencies with concurrency control
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	errChan := make(chan error, len(depsToIngest))
	successCount := 0
	var mu sync.Mutex

	for _, dep := range depsToIngest {
		wg.Add(1)
		go func(d buildlayout.Dependency) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore

			fmt.Printf("Ingesting %s...\n", d.Source)

			// Determine ingestion and fetch types based on dependency type
			ingestionType := depType
			fetchType := mapDepTypeToFetchType(depType)

			err := ingestRepository(routerAddr, d.Source, "", "", ingestionType, fetchType, false)
			if err != nil {
				errChan <- fmt.Errorf("%s: %w", d.Source, err)
				fmt.Printf("Failed: %s: %v\n", d.Source, err)
			} else {
				mu.Lock()
				successCount++
				mu.Unlock()
				fmt.Printf("Success: %s\n", d.Source)
			}
		}(dep)
	}

	wg.Wait()
	close(errChan)

	// Collect errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	fmt.Printf("\nIngestion complete: %d succeeded, %d failed\n", successCount, len(errors))

	if len(errors) > 0 {
		fmt.Println("\nFailed dependencies:")
		for _, err := range errors {
			fmt.Printf("  - %v\n", err)
		}
		return fmt.Errorf("%d dependencies failed to ingest", len(errors))
	}

	return nil
}

// selectMapper returns the appropriate layout mapper for the given type.
func selectMapper(depType string) buildlayout.LayoutMapper {
	switch strings.ToLower(depType) {
	case "go":
		return golangmapper.NewGoMapper()
	default:
		return nil
	}
}

// mapDepTypeToFetchType converts dependency type to fetch type.
func mapDepTypeToFetchType(depType string) string {
	switch strings.ToLower(depType) {
	case "go":
		return "gomod"
	default:
		return "git"
	}
}

// fetchExistingRepos fetches the list of existing repository display paths from the router.
func fetchExistingRepos(routerAddr string) ([]string, error) {
	// Extract HTTP address from gRPC address (convert port 9090 -> 8080)
	httpAddr := strings.Replace(routerAddr, ":9090", ":8080", 1)
	if !strings.Contains(httpAddr, ":") {
		httpAddr = httpAddr + ":8080"
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/api/repos", httpAddr))
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var data struct {
		Repositories []struct {
			DisplayPath string `json:"display_path"`
		} `json:"repositories"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}

	var repos []string
	for _, repo := range data.Repositories {
		repos = append(repos, repo.DisplayPath)
	}

	return repos, nil
}

// ingestRepository ingests a source via the router.
func ingestRepository(routerAddr, rawSource, ref, customSourceID, ingestionType, fetchType string, replicateData bool) error {
	// Validate parameters
	if err := validateIngestionParams(rawSource, ref, ingestionType); err != nil {
		return err
	}

	// For Git, try to parse GitHub URL format if needed
	source := rawSource
	if ingestionType == "git" {
		parsedURL, parsedBranch, err := parseGitHubURL(rawSource)
		if err == nil {
			source = parsedURL
			if ref == "" && parsedBranch != "" {
				ref = parsedBranch
			}
		}
	}

	// Set defaults based on type
	if ref == "" {
		switch ingestionType {
		case "git":
			ref = "main"
		case "go":
			// For Go, version should be in source
			if !strings.Contains(source, "@") {
				return fmt.Errorf("Go module requires version")
			}
		}
	}

	fmt.Printf("Ingesting source: %s", source)
	if ref != "" {
		fmt.Printf(" (ref: %s)", ref)
	}
	fmt.Println()

	// Connect to router
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                1 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	fmt.Printf("Ingesting from router: %s\n", routerAddr)

	// Start ingestion with extended timeout (30 minutes for large sources)
	ingestCtx, ingestCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer ingestCancel()

	stream, err := client.IngestRepository(ingestCtx, &pb.IngestRequest{
		Source:          source,
		Ref:             ref,
		SourceId:        customSourceID, // Empty if not specified, router will auto-generate
		IngestionType:   parseIngestionType(ingestionType),
		FetchType:       parseFetchType(fetchType),
		ReplicateData:   replicateData,
		IngestionConfig: make(map[string]string),
		FetchConfig:     make(map[string]string),
	})
	if err != nil {
		return fmt.Errorf("ingest request failed: %w", err)
	}

	// Receive progress updates
	var lastFilesProcessed int64
	for {
		progress, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("stream error: %w", err)
		}

		// Display progress
		switch progress.Stage {
		case pb.IngestProgress_CLONING:
			fmt.Printf("📥 %s\n", progress.Message)
		case pb.IngestProgress_REGISTERING:
			fmt.Printf("📝 %s\n", progress.Message)
		case pb.IngestProgress_INGESTING:
			if progress.FilesProcessed != lastFilesProcessed {
				fmt.Printf("\r⏳ Ingesting: %d files processed... (current: %s)", progress.FilesProcessed, progress.CurrentFile)
				lastFilesProcessed = progress.FilesProcessed
			}
		case pb.IngestProgress_COMPLETED:
			fmt.Printf("\r✅ %s                    \n", progress.Message)
			return nil
		case pb.IngestProgress_FAILED:
			fmt.Printf("\r❌ Failed: %s\n", progress.Message)
			return fmt.Errorf("ingestion failed: %s", progress.Message)
		}
	}

	return nil
}

// parseIngestionType converts string to IngestionType enum
func parseIngestionType(s string) pb.IngestionType {
	switch strings.ToLower(s) {
	case "git":
		return pb.IngestionType_INGESTION_GIT
	case "go":
		return pb.IngestionType_INGESTION_GO
	case "s3":
		return pb.IngestionType_INGESTION_S3
	case "file":
		return pb.IngestionType_INGESTION_FILE
	default:
		return pb.IngestionType_INGESTION_GIT
	}
}

// parseFetchType converts string to FetchType enum
func parseFetchType(s string) pb.FetchType {
	switch strings.ToLower(s) {
	case "git":
		return pb.FetchType_FETCH_GIT
	case "s3":
		return pb.FetchType_FETCH_S3
	case "local":
		return pb.FetchType_FETCH_LOCAL
	default:
		return pb.FetchType_FETCH_GIT
	}
}

// deleteRepository deletes a repository from all nodes (cleanup to free memory).
func deleteRepository(routerAddr, storageID string) error {
	fmt.Printf("Deleting repository %s from router %s\n", storageID, routerAddr)

	// Connect to router
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                1 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	// Delete with extended timeout (5 minutes max)
	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer deleteCancel()

	resp, err := client.DeleteRepository(deleteCtx, &pb.DeleteRepositoryRequest{
		StorageId: storageID,
	})
	if err != nil {
		return fmt.Errorf("delete request failed: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("deletion failed: %s", resp.Message)
	}

	fmt.Printf("✅ Repository deleted successfully\n")
	fmt.Printf("   Storage ID: %s\n", storageID)
	fmt.Printf("   Files deleted: %d\n", resp.FilesDeleted)
	fmt.Printf("   Message: %s\n", resp.Message)

	return nil
}

// showClusterStatus displays cluster health and node information.
func showClusterStatus(routerAddr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId: "monofs-admin",
	})
	if err != nil {
		return fmt.Errorf("get cluster info: %w", err)
	}

	// Print cluster header
	fmt.Printf("\nCLUSTER STATUS\n")
	fmt.Printf("Router: %s\n", routerAddr)
	fmt.Printf("Cluster ID: %s | Config Version: %d | Total Nodes: %d\n\n", resp.ClusterId, resp.Version, len(resp.Nodes))

	// Calculate stats
	healthyCount := 0
	totalWeight := uint32(0)
	var totalFiles int64
	for _, node := range resp.Nodes {
		if node.Healthy {
			healthyCount++
		}
		totalWeight += node.Weight
		totalFiles += node.TotalFiles
	}

	healthPercent := 0.0
	if len(resp.Nodes) > 0 {
		healthPercent = float64(healthyCount) / float64(len(resp.Nodes)) * 100
	}

	// Health status
	var healthIcon, healthStatus string
	if healthPercent == 100 {
		healthIcon = "[OK]"
		healthStatus = "EXCELLENT"
	} else if healthPercent >= 80 {
		healthIcon = "[!!]"
		healthStatus = "GOOD"
	} else if healthPercent >= 50 {
		healthIcon = "[!!]"
		healthStatus = "DEGRADED"
	} else {
		healthIcon = "[XX]"
		healthStatus = "CRITICAL"
	}

	fmt.Printf("Cluster Health: %s %s (%.0f%%)\n", healthIcon, healthStatus, healthPercent)
	fmt.Printf("Healthy Nodes: %d/%d | Total Weight: %d | Total Files: %s\n\n", healthyCount, len(resp.Nodes), totalWeight, formatNumber(totalFiles))

	// Calculate dynamic column widths
	type rowData struct {
		nodeID       string
		address      string
		healthStatus string
		opStatus     string
		weight       uint32
		health       string
		diskUsage    string
	}

	rows := make([]rowData, 0, len(resp.Nodes))

	// Minimum widths for headers
	maxNodeID := len("Node ID")
	maxAddress := len("Address")
	maxStatus := len("Status")
	maxOpStatus := len("Op Status")
	maxWeight := len("Weight")
	maxHealth := len("Files")
	maxDisk := len("Disk Usage")

	for _, node := range resp.Nodes {
		healthStatus := "HEALTHY"
		if !node.Healthy {
			healthStatus = "UNHEALTHY"
		}

		// Get operational status from node metadata
		opStatus := "Active"
		if node.Metadata != nil {
			if status, ok := node.Metadata["status"]; ok {
				opStatus = status
			}
		}

		nodeID := node.NodeId
		address := node.Address
		health := formatNumber(node.TotalFiles)

		// Format disk usage
		diskUsage := "-"
		if node.DiskFreeBytes > 0 || node.DiskUsedBytes > 0 {
			usedGB := float64(node.DiskUsedBytes) / (1024 * 1024 * 1024)
			freeGB := float64(node.DiskFreeBytes) / (1024 * 1024 * 1024)
			pct := float64(node.DiskUsedBytes) / float64(node.DiskUsedBytes+node.DiskFreeBytes) * 100
			diskUsage = fmt.Sprintf("%.1fGB used / %.1fGB free (%.0f%%)", usedGB, freeGB, pct)
		}

		row := rowData{
			nodeID:       nodeID,
			address:      address,
			healthStatus: healthStatus,
			opStatus:     opStatus,
			weight:       node.Weight,
			health:       health,
			diskUsage:    diskUsage,
		}
		rows = append(rows, row)

		// Calculate max widths
		if len(nodeID) > maxNodeID {
			maxNodeID = len(nodeID)
		}
		if len(address) > maxAddress {
			maxAddress = len(address)
		}
		// Use actual string length for status (emojis are handled by terminal)
		if len(healthStatus) > maxStatus {
			maxStatus = len(healthStatus)
		}
		if len(opStatus) > maxOpStatus {
			maxOpStatus = len(opStatus)
		}
		weightStr := fmt.Sprintf("%d", node.Weight)
		if len(weightStr) > maxWeight {
			maxWeight = len(weightStr)
		}
		if len(health) > maxHealth {
			maxHealth = len(health)
		}
		if len(diskUsage) > maxDisk {
			maxDisk = len(diskUsage)
		}
	}

	// Build format strings
	headerFmt := fmt.Sprintf("║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%ds ║\n",
		maxNodeID, maxAddress, maxStatus, maxOpStatus, maxWeight, maxHealth, maxDisk)
	rowFmt := fmt.Sprintf("║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%ds ║ %%-%dd ║ %%-%ds ║ %%-%ds ║\n",
		maxNodeID, maxAddress, maxStatus, maxOpStatus, maxWeight, maxHealth, maxDisk)

	// Build separator strings
	sep := func(left, mid, right, fill string) string {
		return fmt.Sprintf("%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s\n",
			left,
			strings.Repeat(fill, maxNodeID+2),
			mid,
			strings.Repeat(fill, maxAddress+2),
			mid,
			strings.Repeat(fill, maxStatus+2),
			mid,
			strings.Repeat(fill, maxOpStatus+2),
			mid,
			strings.Repeat(fill, maxWeight+2),
			mid,
			strings.Repeat(fill, maxHealth+2),
			mid,
			strings.Repeat(fill, maxDisk+2),
			right)
	}

	// Print table
	fmt.Print(sep("╔", "╦", "╗", "═"))
	fmt.Printf(headerFmt, "Node ID", "Address", "Status", "Op Status", "Weight", "Files", "Disk Usage")
	fmt.Print(sep("╠", "╬", "╣", "═"))

	for _, row := range rows {
		fmt.Printf(rowFmt,
			row.nodeID,
			row.address,
			row.healthStatus,
			row.opStatus,
			row.weight,
			row.health,
			row.diskUsage,
		)
	}

	fmt.Print(sep("╚", "╩", "╝", "═"))
	fmt.Printf("\n")

	// Additional info if any nodes are unhealthy
	if healthyCount < len(resp.Nodes) {
		fmt.Printf("  [!] WARNING: Some nodes are unhealthy. Check failover status.\n")
		fmt.Printf("      Run: monofs-admin failover --router=%s\n", routerAddr)
		fmt.Printf("\n")
	}

	// Fetch and display Fetcher status summary
	fetcherStats := fetchFetcherStatsSummary(routerAddr)
	if fetcherStats != nil {
		healthIcon := "[OK]"
		if fetcherStats.healthyFetchers < fetcherStats.totalFetchers {
			if fetcherStats.healthyFetchers == 0 {
				healthIcon = "[XX]"
			} else {
				healthIcon = "[!!]"
			}
		}
		fmt.Printf("FETCHERS\n")
		fmt.Printf("%s Fetchers: %d/%d healthy | Cache: %s (%.1f%% hit rate) | Requests: %s\n\n",
			healthIcon,
			fetcherStats.healthyFetchers,
			fetcherStats.totalFetchers,
			formatBytes(fetcherStats.cacheSize),
			fetcherStats.hitRate*100,
			formatNumber(fetcherStats.totalRequests))
	}

	// Fetch and display Search/Indexer status summary
	searchStats := fetchSearchStatsSummary(routerAddr)
	if searchStats != nil {
		healthIcon := "[OK]"
		if searchStats.failedJobs > 0 {
			healthIcon = "[!!]"
		}
		if searchStats.uptime == "" {
			healthIcon = "[XX]"
			fmt.Printf("SEARCH INDEXER\n")
			fmt.Printf("%s Search service unavailable\n\n", healthIcon)
		} else {
			fmt.Printf("SEARCH INDEXER\n")
			fmt.Printf("%s Indexed: %d repos (%s files) | Failed: %d | Searches: %s | Uptime: %s\n\n",
				healthIcon,
				searchStats.totalIndexes,
				formatNumber(searchStats.totalFiles),
				searchStats.failedJobs,
				formatNumber(searchStats.searches),
				searchStats.uptime)
		}
	}

	return nil
}

// fetcherStatsSummary holds summary stats for fetchers
type fetcherStatsSummary struct {
	totalFetchers   int
	healthyFetchers int
	cacheSize       int64
	hitRate         float64
	totalRequests   int64
}

// fetchFetcherStatsSummary fetches fetcher cluster stats via HTTP
func fetchFetcherStatsSummary(routerAddr string) *fetcherStatsSummary {
	// Convert gRPC address (host:9090) to HTTP address (host:8080)
	httpAddr := strings.Replace(routerAddr, ":9090", ":8080", 1)
	apiURL := fmt.Sprintf("http://%s/api/fetchers", httpAddr)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var stats FetcherClusterStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil
	}

	if stats.Error != "" {
		return nil
	}

	return &fetcherStatsSummary{
		totalFetchers:   stats.TotalFetchers,
		healthyFetchers: stats.HealthyFetchers,
		cacheSize:       stats.TotalCacheSizeBytes,
		hitRate:         stats.AggregatedHitRate,
		totalRequests:   stats.TotalRequests,
	}
}

// searchStatsSummary holds summary stats for search/indexer
type searchStatsSummary struct {
	totalIndexes int32
	totalFiles   int64
	failedJobs   int32
	searches     int64
	uptime       string
}

// fetchSearchStatsSummary fetches search stats via HTTP
func fetchSearchStatsSummary(routerAddr string) *searchStatsSummary {
	// Convert gRPC address (host:9090) to HTTP address (host:8080)
	httpAddr := strings.Replace(routerAddr, ":9090", ":8080", 1)
	apiURL := fmt.Sprintf("http://%s/api/search/stats", httpAddr)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return &searchStatsSummary{} // Return empty to show "unavailable"
	}
	defer resp.Body.Close()

	var stats struct {
		TotalIndexes      int32  `json:"total_indexes"`
		TotalFilesIndexed int64  `json:"total_files_indexed"`
		JobsFailed        int32  `json:"jobs_failed"`
		SearchesTotal     int64  `json:"searches_total"`
		Uptime            string `json:"uptime"`
		Error             string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return &searchStatsSummary{}
	}

	if stats.Error != "" {
		return &searchStatsSummary{}
	}

	return &searchStatsSummary{
		totalIndexes: stats.TotalIndexes,
		totalFiles:   stats.TotalFilesIndexed,
		failedJobs:   stats.JobsFailed,
		searches:     stats.SearchesTotal,
		uptime:       stats.Uptime,
	}
}

// showFailoverStatus displays failover mappings and node backup status.
func showFailoverStatus(routerAddr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId: "monofs-admin",
	})
	if err != nil {
		return fmt.Errorf("get cluster info: %w", err)
	}

	// Print header
	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                            REPLICATION STATUS                                 ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")

	// Router info
	fmt.Printf("  [*] Router:         %s\n", routerAddr)

	// Cluster info
	fmt.Printf("  [*] Cluster ID:     %s\n", resp.ClusterId)
	fmt.Printf("  [*] Config Version: %d\n", resp.Version)
	fmt.Printf("\n")

	// Count active and failed nodes
	activeNodes := 0
	failedNodes := 0
	for _, node := range resp.Nodes {
		if node.Healthy {
			activeNodes++
		} else {
			failedNodes++
		}
	}

	fmt.Printf("  [+] Active Nodes:   %d\n", activeNodes)
	fmt.Printf("  [-] Failed Nodes:   %d\n", failedNodes)
	fmt.Printf("  [*] Replication:   2x (Active)\n")
	fmt.Printf("\n")

	if failedNodes == 0 {
		fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
		fmt.Printf("║                                                                               ║\n")
		fmt.Printf("║                     All nodes healthy - No failovers active                   ║\n")
		fmt.Printf("║                                                                               ║\n")
		fmt.Printf("║              All data is accessible on primary nodes only.                    ║\n")
		fmt.Printf("║              Cluster is operating at full capacity.                           ║\n")
		fmt.Printf("║                                                                               ║\n")
		fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	} else {
		fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
		fmt.Printf("║                            FAILOVER MAPPINGS ACTIVE                           ║\n")
		fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
		fmt.Printf("\n")

		// Show failover status for failed nodes
		fmt.Printf("  When a node fails, healthy nodes automatically cover its traffic.\n")
		fmt.Printf("  Backup nodes handle requests using HRW consistent hashing failover.\n")
		fmt.Printf("\n")

		// Build failover mapping
		failoverMap := make(map[string]string)
		for _, node := range resp.Nodes {
			if !node.Healthy {
				// Find which node is backing up this failed node
				// Use HRW to determine backup (same logic as router)
				for _, backupNode := range resp.Nodes {
					if backupNode.Healthy && backupNode.NodeId != node.NodeId {
						failoverMap[node.NodeId] = backupNode.NodeId
						break
					}
				}
			}
		}

		// Display each node's status
		fmt.Printf("╔════════════════╦═══════════════════════╦════════════╦══════════════════════╗\n")
		fmt.Printf("║ Node ID        ║ Address               ║ Status     ║ Replication Info     ║\n")
		fmt.Printf("╠════════════════╬═══════════════════════╬════════════╬══════════════════════╣\n")

		for _, node := range resp.Nodes {
			nodeID := node.NodeId
			if len(nodeID) > 14 {
				nodeID = nodeID[:11] + "..."
			}

			address := node.Address
			if len(address) > 21 {
				address = address[:18] + "..."
			}

			status := "ACTIVE"
			replInfo := "Primary storage"

			if !node.Healthy {
				status = "FAILED"
				if backupNode, ok := failoverMap[node.NodeId]; ok {
					replInfo = "→ " + backupNode
					if len(replInfo) > 20 {
						replInfo = "→ " + backupNode[:14] + "..."
					}
				} else {
					replInfo = "No backup found"
				}
			}

			fmt.Printf("║ %-14s ║ %-21s ║ %-10s ║ %-20s ║\n",
				nodeID,
				address,
				status,
				replInfo,
			)
		}

		fmt.Printf("╚════════════════╩═══════════════════════╩════════════╩══════════════════════╝\n")
		fmt.Printf("\n")

		fmt.Printf("  [i] What happens during failover:\n")
		fmt.Printf("      • Failed node's requests are redirected to backup nodes\n")
		fmt.Printf("      • Backup nodes use HRW to determine correct alternative node\n")
		fmt.Printf("      • Data remains accessible with minimal latency impact\n")
		fmt.Printf("      • When failed node recovers, it reclaims its files\n")
		fmt.Printf("\n")
		fmt.Printf("  [i] Tip: Check the web UI for detailed replication metrics:\n")
		fmt.Printf("      http://%s/replication\n", routerAddr)
	}

	fmt.Printf("\n")
	return nil
}

func rebuildDirectoryIndex(routerAddr, storageID string, useExternal, debug bool) error {
	if storageID == "" {
		return fmt.Errorf("storage-id is required")
	}

	// Connect to router
	if debug {
		fmt.Printf("Connecting to router at %s...\n", routerAddr)
	}

	conn, err := grpc.Dial(routerAddr, grpc.WithInsecure())
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	routerClient := pb.NewMonoFSRouterClient(conn)
	ctx := context.Background()

	// Get cluster information to find all nodes
	if debug {
		fmt.Printf("Fetching cluster information...\n")
	}

	clusterResp, err := routerClient.GetClusterInfo(ctx, &pb.ClusterInfoRequest{})
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	if len(clusterResp.Nodes) == 0 {
		return fmt.Errorf("no nodes found in cluster")
	}

	fmt.Printf("Found %d nodes in cluster\n", len(clusterResp.Nodes))
	fmt.Printf("Rebuilding directory indexes for storage ID: %s\n\n", storageID)

	// Map internal addresses to external ones for outside-Docker usage
	externalMap := map[string]string{
		"node1:9000": "localhost:9001",
		"node2:9000": "localhost:9002",
		"node3:9000": "localhost:9003",
		"node4:9000": "localhost:9004",
		"node5:9000": "localhost:9005",
	}

	// Call BuildDirectoryIndexes on each node
	totalDirectories := 0
	failedNodes := 0

	for _, node := range clusterResp.Nodes {
		// Choose address based on external flag
		nodeAddr := node.Address
		if useExternal {
			if extAddr, ok := externalMap[node.Address]; ok {
				nodeAddr = extAddr
			}
		}

		if debug {
			fmt.Printf("Connecting to node %s at %s...\n", node.NodeId, nodeAddr)
		}

		nodeConn, err := grpc.Dial(nodeAddr, grpc.WithInsecure())
		if err != nil {
			fmt.Printf("❌ Failed to connect to node %s: %v\n", node.NodeId, err)
			failedNodes++
			continue
		}

		nodeClient := pb.NewMonoFSClient(nodeConn)

		if debug {
			fmt.Printf("Calling BuildDirectoryIndexes on node %s...\n", node.NodeId)
		}

		buildResp, err := nodeClient.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
		nodeConn.Close()

		if err != nil {
			fmt.Printf("❌ Node %s: Failed - %v\n", node.NodeId, err)
			failedNodes++
		} else {
			fmt.Printf("✅ Node %s: Indexed %d directories\n", node.NodeId, buildResp.DirectoriesIndexed)
			totalDirectories += int(buildResp.DirectoriesIndexed)
		}
	}

	fmt.Printf("\n")
	fmt.Printf("Summary:\n")
	fmt.Printf("  Total directories indexed: %d\n", totalDirectories)
	fmt.Printf("  Successful nodes: %d\n", len(clusterResp.Nodes)-failedNodes)
	fmt.Printf("  Failed nodes: %d\n", failedNodes)

	if failedNodes > 0 {
		return fmt.Errorf("rebuild completed with %d failed nodes", failedNodes)
	}

	return nil
}

// showRepositories displays all repositories in the cluster via HTTP API.
func showRepositories(routerAddr string) error {
	// Use HTTP API to get repositories (same as UI)
	url := fmt.Sprintf("http://%s/api/repositories", routerAddr)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch repositories: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error: %s - %s", resp.Status, string(body))
	}

	var data struct {
		Repositories []struct {
			StorageID         string  `json:"storage_id"`
			RepoID            string  `json:"repo_id"`
			RepoURL           string  `json:"repo_url"`
			Branch            string  `json:"branch"`
			FilesCount        int64   `json:"files_count"`
			IngestedAt        int64   `json:"ingested_at"`
			TopologyVersion   int64   `json:"topology_version"`
			RebalanceState    string  `json:"rebalance_state"`
			RebalanceProgress float64 `json:"rebalance_progress"`
			InProgress        bool    `json:"in_progress"`
			Stage             string  `json:"stage"`
			Message           string  `json:"message"`
		} `json:"repositories"`
		CurrentTopologyVersion int64 `json:"current_topology_version"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	// Print header
	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                              REPOSITORIES                                      ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")

	fmt.Printf("  [*] Router:             %s\n", routerAddr)
	fmt.Printf("  [*] Topology Version:   %d\n", data.CurrentTopologyVersion)
	fmt.Printf("  [*] Total Repositories: %d\n", len(data.Repositories))
	fmt.Printf("\n")

	if len(data.Repositories) == 0 {
		fmt.Printf("  No repositories ingested yet.\n")
		fmt.Printf("  Use: monofs-admin ingest --url=<git-url>\n")
		fmt.Printf("\n")
		return nil
	}

	// Sort repositories by repo ID
	repos := data.Repositories
	sort.Slice(repos, func(i, j int) bool {
		return repos[i].RepoID < repos[j].RepoID
	})

	// Print each repository
	for i, repo := range repos {
		statusIcon := "✅"
		if repo.InProgress {
			statusIcon = "⏳"
		} else if repo.RebalanceState != "Stable" && repo.RebalanceState != "" {
			statusIcon = "🔄"
		}

		fmt.Printf("╔══════════════════════════════════════════════════════════════════════════════╗\n")
		fmt.Printf("║ %s Repository %d: %-60s ║\n", statusIcon, i+1, truncate(repo.RepoID, 60))
		fmt.Printf("╠══════════════════════════════════════════════════════════════════════════════╣\n")
		fmt.Printf("║  Storage ID:    %-60s ║\n", repo.StorageID)
		fmt.Printf("║  URL:           %-60s ║\n", truncate(repo.RepoURL, 60))
		fmt.Printf("║  Branch:        %-60s ║\n", repo.Branch)
		fmt.Printf("║  Files:         %-60d ║\n", repo.FilesCount)

		if repo.IngestedAt > 0 {
			ingestedTime := time.Unix(repo.IngestedAt, 0)
			fmt.Printf("║  Ingested:      %-60s ║\n", ingestedTime.Format(time.RFC3339))
		}

		stateInfo := fmt.Sprintf("%s (%.0f%%)", repo.RebalanceState, repo.RebalanceProgress*100)
		fmt.Printf("║  Rebalance:     %-60s ║\n", stateInfo)

		if repo.InProgress {
			fmt.Printf("║  Stage:         %-60s ║\n", repo.Stage)
			fmt.Printf("║  Message:       %-60s ║\n", truncate(repo.Message, 60))
		}

		fmt.Printf("╚══════════════════════════════════════════════════════════════════════════════╝\n")
		fmt.Printf("\n")
	}

	// Footer with tips
	fmt.Printf("  [i] Commands:\n")
	fmt.Printf("      Rebalance a repo:  monofs-admin rebalance --storage-id=<hash>\n")
	fmt.Printf("      Rebuild indexes:   monofs-admin rebuild-index --storage-id=<hash>\n")
	fmt.Printf("\n")

	return nil
}

// triggerRebalance triggers rebalancing for a repository via HTTP API.
func triggerRebalance(routerAddr, storageID string) error {
	url := fmt.Sprintf("http://%s/api/rebalance", routerAddr)

	data := strings.NewReader(fmt.Sprintf("storage_id=%s", storageID))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/x-www-form-urlencoded", data)
	if err != nil {
		return fmt.Errorf("failed to trigger rebalance: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		State   string `json:"state"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("rebalance failed: %s", result.Message)
	}

	fmt.Printf("✅ %s\n", result.Message)
	fmt.Printf("\n")
	fmt.Printf("  [i] Rebalancing started for storage ID: %s\n", storageID)
	fmt.Printf("  [i] Check progress with: monofs-admin repos --router=%s\n", routerAddr)
	fmt.Printf("\n")

	return nil
}

// showStatistics displays cluster or node statistics
func showStatistics(routerAddr, statsType, format string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create gRPC connection
	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	switch statsType {
	case "cluster":
		return displayClusterStats(ctx, client, format)
	case "nodes":
		return displayNodeStats(ctx, client, format)
	case "all":
		if err := displayClusterStats(ctx, client, format); err != nil {
			return err
		}
		fmt.Println()
		return displayNodeStats(ctx, client, format)
	default:
		return fmt.Errorf("unknown stats type: %s (must be cluster, nodes, or all)", statsType)
	}
}

// displayClusterStats shows cluster-wide statistics
func displayClusterStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error {
	resp, err := client.GetClusterStats(ctx, &pb.ClusterStatsRequest{})
	if err != nil {
		return fmt.Errorf("failed to get cluster stats: %w", err)
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	// Table format
	fmt.Println("=== Cluster Statistics ===")
	fmt.Printf("Total Nodes:         %d\n", resp.TotalNodes)
	fmt.Printf("  Healthy:           %d\n", resp.HealthyNodes)
	fmt.Printf("  Unhealthy:         %d\n", resp.UnhealthyNodes)
	fmt.Printf("  Syncing:           %d\n", resp.SyncingNodes)
	fmt.Printf("Total Repositories:  %d\n", resp.TotalRepositories)
	fmt.Printf("Total Files:         %s\n", formatNumber(resp.TotalFiles))
	fmt.Printf("Total Size:          %s\n", formatBytes(resp.TotalSizeBytes))
	fmt.Printf("Cluster Version:     %d\n", resp.ClusterVersion)

	if len(resp.Failovers) > 0 {
		fmt.Println("\nActive Failovers:")
		for source, target := range resp.Failovers {
			fmt.Printf("  %s -> %s\n", source, target)
		}
	}

	return nil
}

// displayNodeStats shows per-node statistics
func displayNodeStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error {
	resp, err := client.GetNodeStats(ctx, &pb.NodeStatsRequest{})
	if err != nil {
		return fmt.Errorf("failed to get node stats: %w", err)
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	// Table format
	fmt.Println("=== Node Statistics ===")
	fmt.Printf("%-20s %-15s %-10s %-8s %-15s %-10s\n",
		"NODE ID", "ADDRESS", "STATUS", "HEALTHY", "FILES", "SYNC %")
	fmt.Println(strings.Repeat("-", 100))

	for _, node := range resp.Nodes {
		healthStr := "NO"
		if node.Healthy {
			healthStr = "YES"
		}

		syncPct := ""
		if node.Status == "Syncing" {
			syncPct = fmt.Sprintf("%.1f%%", node.SyncProgress*100)
		}

		fmt.Printf("%-20s %-15s %-10s %-8s %-15s %-10s\n",
			truncate(node.NodeId, 20),
			node.Address,
			node.Status,
			healthStr,
			formatNumber(node.FileCount),
			syncPct,
		)

		if len(node.BackingUpNodes) > 0 {
			fmt.Printf("  Backing up: %s\n", strings.Join(node.BackingUpNodes, ", "))
		}
	}

	return nil
}

// formatBytes converts bytes to human-readable format
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// formatNumber formats large numbers with commas
func formatNumber(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// truncate shortens a string to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// triggerFailover manually triggers graceful failover for a node
func triggerFailover(routerAddr, nodeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                         TRIGGERING FAILOVER                                   ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("  [*] Router:   %s\n", routerAddr)
	fmt.Printf("  [*] Node ID:  %s\n", nodeID)
	fmt.Printf("\n")

	resp, err := client.RequestFailover(ctx, &pb.FailoverRequest{
		SourceNodeId: nodeID,
		Timestamp:    time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("failed to request failover: %w", err)
	}

	if resp.Success {
		fmt.Printf("  [✓] Failover triggered successfully\n")
		fmt.Printf("  [*] Target node: %s\n", resp.TargetNodeId)
		fmt.Printf("  [i] Message: %s\n", resp.Message)
	} else {
		fmt.Printf("  [✗] Failover failed\n")
		fmt.Printf("  [i] Message: %s\n", resp.Message)
	}

	fmt.Printf("\n")
	fmt.Printf("  [i] Check failover status with: monofs-admin failover --router=%s\n", routerAddr)
	fmt.Printf("\n")

	return nil
}

// clearFailover clears failover state for a recovered node
func clearFailover(routerAddr, nodeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First, get cluster info to find the node's address
	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	routerClient := pb.NewMonoFSRouterClient(conn)

	clusterInfo, err := routerClient.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId: "monofs-admin",
	})
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	// Find the node address
	var nodeAddr string
	for _, node := range clusterInfo.Nodes {
		if node.NodeId == nodeID {
			nodeAddr = node.Address
			break
		}
	}

	if nodeAddr == "" {
		return fmt.Errorf("node %s not found in cluster", nodeID)
	}

	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                        CLEARING FAILOVER CACHE                                ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("  [*] Router:       %s\n", routerAddr)
	fmt.Printf("  [*] Node ID:      %s\n", nodeID)
	fmt.Printf("  [*] Node Address: %s\n", nodeAddr)
	fmt.Printf("\n")

	// Connect to the node directly to clear its failover cache
	nodeConn, err := grpc.DialContext(ctx, nodeAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to node: %w", err)
	}
	defer nodeConn.Close()

	nodeClient := pb.NewMonoFSClient(nodeConn)

	resp, err := nodeClient.ClearFailoverCache(ctx, &pb.ClearFailoverCacheRequest{
		RecoveredNodeId: nodeID,
	})
	if err != nil {
		return fmt.Errorf("failed to clear failover cache: %w", err)
	}

	fmt.Printf("  [✓] Failover cache cleared\n")
	fmt.Printf("  [*] Entries cleared: %d\n", resp.EntriesCleared)
	fmt.Printf("\n")
	fmt.Printf("  [i] The node is now serving only its own data\n")
	fmt.Printf("  [i] Check cluster status with: monofs-admin status --router=%s\n", routerAddr)
	fmt.Printf("\n")

	return nil
}

// showNodeFiles lists files owned by a specific node (or all nodes if nodeID="all")
func showNodeFiles(routerAddr, nodeID, storageID, format string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First, get cluster info to find the node's address
	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	routerClient := pb.NewMonoFSRouterClient(conn)

	clusterInfo, err := routerClient.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId: "monofs-admin",
	})
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	// Handle "all" node ID - query all nodes
	if nodeID == "all" {
		if storageID == "" {
			return fmt.Errorf("--storage-id is required when using --node-id=all")
		}
		return showAllNodesFiles(ctx, routerAddr, clusterInfo, storageID, format)
	}

	// Find the node address
	var nodeAddr string
	for _, node := range clusterInfo.Nodes {
		if node.NodeId == nodeID {
			nodeAddr = node.Address
			break
		}
	}

	if nodeAddr == "" {
		return fmt.Errorf("node %s not found in cluster", nodeID)
	}

	// Connect to the node directly
	nodeConn, err := grpc.DialContext(ctx, nodeAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to node: %w", err)
	}
	defer nodeConn.Close()

	nodeClient := pb.NewMonoFSClient(nodeConn)

	resp, err := nodeClient.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
		StorageId: storageID,
	})
	if err != nil {
		return fmt.Errorf("failed to get repository files: %w", err)
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	// Table format
	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                         NODE FILES (Single Node)                              ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("  [*] Router:       %s\n", routerAddr)
	fmt.Printf("  [*] Node ID:      %s\n", nodeID)
	fmt.Printf("  [*] Node Address: %s\n", nodeAddr)
	if storageID != "" {
		fmt.Printf("  [*] Storage ID:   %s\n", storageID)
	}
	fmt.Printf("  [*] Total Files:  %d (on this node only)\n", len(resp.Files))
	fmt.Printf("\n")
	fmt.Printf("  [i] NOTE: Files are sharded across nodes. Use --node-id=all to see all files.\n")
	fmt.Printf("\n")

	if len(resp.Files) == 0 {
		fmt.Printf("  [i] No files found on this node\n")
		fmt.Printf("\n")
		return nil
	}

	// Group files by storage ID
	filesByStorage := make(map[string][]string)
	for _, file := range resp.Files {
		parts := strings.SplitN(file, "/", 2)
		if len(parts) == 2 {
			sid := parts[0]
			path := parts[1]
			filesByStorage[sid] = append(filesByStorage[sid], path)
		} else {
			filesByStorage["unknown"] = append(filesByStorage["unknown"], file)
		}
	}

	// Display grouped results
	for sid, files := range filesByStorage {
		fmt.Printf("  Storage: %s (%d files)\n", sid, len(files))
		// Show first 10 files
		displayed := 0
		for _, path := range files {
			if displayed >= 10 {
				fmt.Printf("    ... and %d more files\n", len(files)-10)
				break
			}
			fmt.Printf("    %s\n", path)
			displayed++
		}
		fmt.Printf("\n")
	}

	return nil
}

// showAllNodesFiles lists files for a storage ID across ALL nodes
func showAllNodesFiles(ctx context.Context, routerAddr string, clusterInfo *pb.ClusterInfoResponse, storageID, format string) error {
	type nodeFiles struct {
		nodeID string
		files  []string
		err    error
	}

	// Query all nodes in parallel
	results := make(chan nodeFiles, len(clusterInfo.Nodes))

	for _, node := range clusterInfo.Nodes {
		go func(n *pb.NodeInfo) {
			nodeConn, err := grpc.DialContext(ctx, n.Address,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
			)
			if err != nil {
				results <- nodeFiles{nodeID: n.NodeId, err: err}
				return
			}
			defer nodeConn.Close()

			nodeClient := pb.NewMonoFSClient(nodeConn)
			resp, err := nodeClient.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
				StorageId: storageID,
			})
			if err != nil {
				results <- nodeFiles{nodeID: n.NodeId, err: err}
				return
			}

			results <- nodeFiles{nodeID: n.NodeId, files: resp.Files}
		}(node)
	}

	// Collect results
	allFiles := make(map[string][]string)  // file -> nodes that have it
	nodeFileCounts := make(map[string]int) // node -> file count
	var errors []string

	for i := 0; i < len(clusterInfo.Nodes); i++ {
		result := <-results
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.nodeID, result.err))
			continue
		}
		nodeFileCounts[result.nodeID] = len(result.files)
		for _, file := range result.files {
			// Strip storage ID prefix if present
			path := file
			if strings.HasPrefix(file, storageID+"/") {
				path = strings.TrimPrefix(file, storageID+"/")
			}
			allFiles[path] = append(allFiles[path], result.nodeID)
		}
	}

	if format == "json" {
		output := map[string]interface{}{
			"storage_id":       storageID,
			"total_files":      len(allFiles),
			"node_file_counts": nodeFileCounts,
			"files":            allFiles,
			"errors":           errors,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Table format
	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                       REPOSITORY FILES (All Nodes)                            ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("  [*] Router:        %s\n", routerAddr)
	fmt.Printf("  [*] Storage ID:    %s\n", storageID)
	fmt.Printf("  [*] Total Files:   %d (across all nodes)\n", len(allFiles))
	fmt.Printf("  [*] Nodes Queried: %d\n", len(clusterInfo.Nodes))
	fmt.Printf("\n")

	// Show files per node
	fmt.Printf("  Files per Node:\n")
	for _, node := range clusterInfo.Nodes {
		count := nodeFileCounts[node.NodeId]
		fmt.Printf("    %s: %d files\n", node.NodeId, count)
	}
	fmt.Printf("\n")

	if len(errors) > 0 {
		fmt.Printf("  [!] Errors:\n")
		for _, e := range errors {
			fmt.Printf("    - %s\n", e)
		}
		fmt.Printf("\n")
	}

	if len(allFiles) == 0 {
		fmt.Printf("  [i] No files found for this storage ID\n")
		fmt.Printf("\n")
		return nil
	}

	// Show first 20 files with their locations
	fmt.Printf("  Files (showing first 20):\n")
	count := 0
	for path, nodes := range allFiles {
		if count >= 20 {
			fmt.Printf("    ... and %d more files\n", len(allFiles)-20)
			break
		}
		fmt.Printf("    %s  [%s]\n", path, strings.Join(nodes, ", "))
		count++
	}
	fmt.Printf("\n")

	return nil
}

// FetcherClusterStats represents the response from the fetchers API
type FetcherClusterStats struct {
	TotalFetchers        int            `json:"total_fetchers"`
	HealthyFetchers      int            `json:"healthy_fetchers"`
	TotalRequests        int64          `json:"total_requests"`
	TotalCacheHits       int64          `json:"total_cache_hits"`
	TotalCacheMisses     int64          `json:"total_cache_misses"`
	AggregatedHitRate    float64        `json:"aggregated_hit_rate"`
	TotalCacheSizeBytes  int64          `json:"total_cache_size_bytes"`
	TotalCacheEntries    int64          `json:"total_cache_entries"`
	TotalActiveFetches   int64          `json:"total_active_fetches"`
	TotalQueuedPrefetch  int64          `json:"total_queued_prefetch"`
	TotalBytesFetched    int64          `json:"total_bytes_fetched"`
	TotalBytesServed     int64          `json:"total_bytes_served"`
	Fetchers             []FetcherStats `json:"fetchers"`
	ClientAffinityHits   int64          `json:"client_affinity_hits"`
	ClientAffinityMisses int64          `json:"client_affinity_misses"`
	ClientTotalRequests  int64          `json:"client_total_requests"`
	Error                string         `json:"error,omitempty"`
	Available            bool           `json:"available,omitempty"`
}

// FetcherStats represents a single fetcher instance
type FetcherStats struct {
	Address          string                     `json:"address"`
	FetcherID        string                     `json:"fetcher_id"`
	Healthy          bool                       `json:"healthy"`
	UptimeSeconds    int64                      `json:"uptime_seconds"`
	TotalRequests    int64                      `json:"total_requests"`
	CacheHits        int64                      `json:"cache_hits"`
	CacheMisses      int64                      `json:"cache_misses"`
	CacheHitRate     float64                    `json:"cache_hit_rate"`
	CacheSizeBytes   int64                      `json:"cache_size_bytes"`
	CacheEntries     int64                      `json:"cache_entries"`
	ActiveFetches    int64                      `json:"active_fetches"`
	QueuedPrefetches int64                      `json:"queued_prefetches"`
	BytesFetched     int64                      `json:"bytes_fetched"`
	BytesServed      int64                      `json:"bytes_served"`
	SourceStats      map[string]SourceStatsInfo `json:"source_stats,omitempty"`
	ErrorCount       int64                      `json:"error_count"`
	LastError        string                     `json:"last_error,omitempty"`
}

// SourceStatsInfo for per-source statistics
type SourceStatsInfo struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	BytesFetched int64   `json:"bytes_fetched"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	CachedItems  int64   `json:"cached_items"`
}

func showFetchers(routerHTTP, format string, detailed bool) error {
	// Build URL
	apiURL := fmt.Sprintf("http://%s/api/fetchers", routerHTTP)
	if detailed {
		apiURL += "?detailed=true"
	}

	resp, err := http.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer resp.Body.Close()

	var stats FetcherClusterStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if stats.Error != "" {
		return fmt.Errorf("fetcher cluster error: %s", stats.Error)
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stats)
	}

	// Table format
	fmt.Printf("\n")
	fmt.Printf("╔═══════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                           FETCHER CLUSTER STATUS                              ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")

	// Overview section
	healthPct := float64(0)
	if stats.TotalFetchers > 0 {
		healthPct = float64(stats.HealthyFetchers) / float64(stats.TotalFetchers) * 100
	}
	healthStatus := "[OK]"
	if healthPct < 100 && healthPct >= 50 {
		healthStatus = "[!!]"
	} else if healthPct < 50 {
		healthStatus = "[XX]"
	}

	fmt.Printf("  CLUSTER OVERVIEW\n")
	fmt.Printf("  %s Fetchers: %d/%d (%.0f%% healthy)\n",
		healthStatus, stats.HealthyFetchers, stats.TotalFetchers, healthPct)
	fmt.Printf("  Cache Hit Rate:   %.1f%%\n", stats.AggregatedHitRate*100)
	fmt.Printf("  Cache Size:       %s (%d entries)\n",
		formatBytes(stats.TotalCacheSizeBytes), stats.TotalCacheEntries)
	fmt.Printf("  Total Requests:   %s\n", formatNumber(stats.TotalRequests))
	fmt.Printf("  Bytes Fetched:    %s\n", formatBytes(stats.TotalBytesFetched))
	fmt.Printf("  Bytes Served:     %s\n", formatBytes(stats.TotalBytesServed))
	fmt.Printf("\n")

	// Fetcher instances table
	fmt.Printf("  FETCHER INSTANCES\n")
	fmt.Printf("  %-22s %-8s %12s %10s %10s %8s\n",
		"FETCHER", "STATUS", "REQUESTS", "HIT RATE", "CACHE", "ACTIVE")
	fmt.Printf("  %-22s %-8s %12s %10s %10s %8s\n",
		strings.Repeat("-", 22), strings.Repeat("-", 8), strings.Repeat("-", 12),
		strings.Repeat("-", 10), strings.Repeat("-", 10), strings.Repeat("-", 8))

	for _, f := range stats.Fetchers {
		status := "UP"
		if !f.Healthy {
			status = "DOWN"
		}
		hitRate := "0.0%"
		total := f.CacheHits + f.CacheMisses
		if total > 0 {
			hitRate = fmt.Sprintf("%.1f%%", float64(f.CacheHits)/float64(total)*100)
		}
		fetcherName := f.FetcherID
		if fetcherName == "" {
			fetcherName = f.Address
		}
		if len(fetcherName) > 22 {
			fetcherName = fetcherName[:19] + "..."
		}

		fmt.Printf("  %-22s %-8s %12d %10s %10s %8d\n",
			fetcherName, status, f.TotalRequests, hitRate,
			formatBytesShort(f.CacheSizeBytes), f.ActiveFetches)
	}
	fmt.Printf("\n")

	// Detailed per-source stats if requested
	if detailed {
		for _, f := range stats.Fetchers {
			if len(f.SourceStats) > 0 {
				fmt.Printf("  ┌─ SOURCE STATS: %s\n", f.FetcherID)
				for source, ss := range f.SourceStats {
					fmt.Printf("  │  %-12s  Requests: %-8d  Errors: %-4d  Avg: %.1fms\n",
						source, ss.Requests, ss.Errors, ss.AvgLatencyMs)
				}
				fmt.Printf("  └───────────────────────────────────────────────────────────────────────────────┘\n")
			}
		}
		fmt.Printf("\n")
	}

	// Client routing stats
	if stats.ClientTotalRequests > 0 {
		affinityRate := float64(stats.ClientAffinityHits) / float64(stats.ClientTotalRequests) * 100
		fmt.Printf("  [i] Client Routing: %d requests, %.1f%% affinity hit rate\n",
			stats.ClientTotalRequests, affinityRate)
		fmt.Printf("\n")
	}

	return nil
}

func formatBytesShort(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
