// MonoFS Load Test - Generate filesystem load for testing
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mountPath    = flag.String("mount", "/mnt/monofs", "Path to mounted MonoFS filesystem")
	duration     = flag.Duration("duration", 60*time.Second, "Test duration")
	concurrency  = flag.Int("concurrency", 10, "Number of concurrent workers")
	fileSize     = flag.Int("file-size", 1024, "File size in bytes (for write operations)")
	readRatio    = flag.Float64("read-ratio", 0.5, "Ratio of read operations (0.0-1.0)")
	writeRatio   = flag.Float64("write-ratio", 0.3, "Ratio of write operations (0.0-1.0)")
	deleteRatio  = flag.Float64("delete-ratio", 0.1, "Ratio of delete operations (0.0-1.0)")
	mkdirRatio   = flag.Float64("mkdir-ratio", 0.05, "Ratio of mkdir operations (0.0-1.0)")
	listRatio    = flag.Float64("list-ratio", 0.05, "Ratio of list operations (0.0-1.0)")
	verbose      = flag.Bool("verbose", false, "Enable verbose logging")
	readExisting = flag.Bool("read-existing", false, "Read existing files from repositories instead of created files")
	repoPath     = flag.String("repo-path", "", "Path to repository for reading existing files (relative to mount)")
	maxScanFiles = flag.Int("max-scan-files", 10000, "Maximum number of files to scan for read-existing mode")
	readOnlyMode = flag.Bool("read-only", false, "Run in read-only mode (only read existing files, no writes)")
)

// Statistics counters
type Stats struct {
	reads        atomic.Int64
	writes       atomic.Int64
	deletes      atomic.Int64
	mkdirs       atomic.Int64
	lists        atomic.Int64
	errors       atomic.Int64
	totalOps     atomic.Int64
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
}

func main() {
	flag.Parse()

	// Handle read-only mode
	if *readOnlyMode {
		*readRatio = 1.0
		*writeRatio = 0.0
		*deleteRatio = 0.0
		*mkdirRatio = 0.0
		*listRatio = 0.0
		*readExisting = true // read-only implies reading existing files
	}

	// Validate ratios
	totalRatio := *readRatio + *writeRatio + *deleteRatio + *mkdirRatio + *listRatio
	if totalRatio > 1.0 {
		log.Fatal("Sum of all ratios must not exceed 1.0")
	}

	// Check if mount path exists
	if _, err := os.Stat(*mountPath); os.IsNotExist(err) {
		log.Fatalf("Mount path does not exist: %s", *mountPath)
	}

	// Scan existing files if read-existing mode is enabled
	var existingFiles []string
	if *readExisting {
		scanPath := *mountPath
		if *repoPath != "" {
			scanPath = filepath.Join(*mountPath, *repoPath)
		}
		fmt.Printf("Scanning existing files from: %s\n", scanPath)
		existingFiles = scanExistingFiles(scanPath, *maxScanFiles)
		if len(existingFiles) == 0 {
			log.Fatalf("No files found to read in: %s", scanPath)
		}
		fmt.Printf("Found %d files for read testing\n", len(existingFiles))
	}

	// Create test directory (only if we're doing writes)
	var testDir string
	if *writeRatio > 0 || *mkdirRatio > 0 {
		testDir = filepath.Join(*mountPath, fmt.Sprintf("loadtest-%d", time.Now().Unix()))
		if err := os.MkdirAll(testDir, 0755); err != nil {
			log.Fatalf("Failed to create test directory: %v", err)
		}
		defer cleanup(testDir)
	}

	fmt.Printf("MonoFS Load Test\n")
	fmt.Printf("================\n")
	fmt.Printf("Mount Path:    %s\n", *mountPath)
	if testDir != "" {
		fmt.Printf("Test Dir:      %s\n", testDir)
	}
	fmt.Printf("Duration:      %s\n", *duration)
	fmt.Printf("Concurrency:   %d workers\n", *concurrency)
	fmt.Printf("File Size:     %d bytes\n", *fileSize)
	fmt.Printf("Read Ratio:    %.2f\n", *readRatio)
	fmt.Printf("Write Ratio:   %.2f\n", *writeRatio)
	fmt.Printf("Delete Ratio:  %.2f\n", *deleteRatio)
	fmt.Printf("Mkdir Ratio:   %.2f\n", *mkdirRatio)
	fmt.Printf("List Ratio:    %.2f\n", *listRatio)
	if *readExisting {
		fmt.Printf("Read Mode:     existing files (%d files)\n", len(existingFiles))
	} else {
		fmt.Printf("Read Mode:     created files only\n")
	}
	fmt.Printf("\n")

	stats := &Stats{}
	startTime := time.Now()
	stopChan := make(chan struct{})
	var wg sync.WaitGroup

	// Start progress reporter
	go reportProgress(stats, startTime, stopChan)

	// Start workers
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(workerID, testDir, existingFiles, stats, stopChan)
		}(i)
	}

	// Run for specified duration
	time.Sleep(*duration)
	close(stopChan)
	wg.Wait()

	// Final report
	elapsed := time.Since(startTime)
	printFinalReport(stats, elapsed)
}

// runWorker executes random filesystem operations
func runWorker(id int, testDir string, existingFiles []string, stats *Stats, stopChan <-chan struct{}) {
	var workerDir string
	if testDir != "" {
		workerDir = filepath.Join(testDir, fmt.Sprintf("worker-%d", id))
		if err := os.MkdirAll(workerDir, 0755); err != nil {
			log.Printf("Worker %d: failed to create directory: %v", id, err)
			return
		}
	}

	fileCounter := 0
	createdFiles := make([]string, 0)

	for {
		select {
		case <-stopChan:
			return
		default:
			// Determine operation based on ratios
			r := randFloat()
			var err error

			switch {
			case r < *readRatio:
				// Read operation - prefer existing files if available
				if *readExisting && len(existingFiles) > 0 {
					file := existingFiles[randInt(len(existingFiles))]
					err = readFile(file, stats)
				} else if len(createdFiles) > 0 {
					file := createdFiles[randInt(len(createdFiles))]
					err = readFile(file, stats)
				} else {
					// Skip read if no files exist
					continue
				}

			case r < *readRatio+*writeRatio:
				// Write operation
				if workerDir == "" {
					continue // No test dir for writes
				}
				filename := filepath.Join(workerDir, fmt.Sprintf("file-%d.dat", fileCounter))
				err = writeFile(filename, *fileSize, stats)
				if err == nil {
					createdFiles = append(createdFiles, filename)
					fileCounter++
				}

			case r < *readRatio+*writeRatio+*deleteRatio:
				// Delete operation - only if we have files
				if len(createdFiles) > 0 {
					idx := randInt(len(createdFiles))
					file := createdFiles[idx]
					err = deleteFile(file, stats)
					if err == nil {
						createdFiles = append(createdFiles[:idx], createdFiles[idx+1:]...)
					}
				} else {
					// Skip delete if no files exist
					continue
				}

			case r < *readRatio+*writeRatio+*deleteRatio+*mkdirRatio:
				// Mkdir operation
				if workerDir == "" {
					continue // No test dir for mkdir
				}
				dirName := filepath.Join(workerDir, fmt.Sprintf("dir-%d", fileCounter))
				err = mkdirOp(dirName, stats)

			case r < *readRatio+*writeRatio+*deleteRatio+*mkdirRatio+*listRatio:
				// List operation
				listPath := workerDir
				if listPath == "" && *repoPath != "" {
					listPath = filepath.Join(*mountPath, *repoPath)
				} else if listPath == "" {
					listPath = *mountPath
				}
				err = listDir(listPath, stats)
			}

			if err != nil {
				stats.errors.Add(1)
				if *verbose {
					log.Printf("Worker %d: operation failed: %v", id, err)
				}
				// Add backoff on error to avoid hammering
				time.Sleep(10 * time.Millisecond)
			} else {
				stats.totalOps.Add(1)
			}

			// Small delay to avoid overwhelming the system
			time.Sleep(time.Millisecond)
		}
	}
}

// readFile reads a file and updates statistics
func readFile(path string, stats *Stats) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	stats.reads.Add(1)
	stats.bytesRead.Add(int64(len(data)))
	return nil
}

// writeFile writes a file with random data
func writeFile(path string, size int, stats *Stats) error {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}

	stats.writes.Add(1)
	stats.bytesWritten.Add(int64(size))
	return nil
}

// deleteFile deletes a file
func deleteFile(path string, stats *Stats) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	stats.deletes.Add(1)
	return nil
}

// mkdirOp creates a directory
func mkdirOp(path string, stats *Stats) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	stats.mkdirs.Add(1)
	return nil
}

// listDir lists directory contents
func listDir(path string, stats *Stats) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	stats.lists.Add(1)
	if *verbose {
		log.Printf("Listed %d entries in %s", len(entries), path)
	}
	return nil
}

// reportProgress prints periodic progress updates
func reportProgress(stats *Stats, startTime time.Time, stopChan <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			elapsed := time.Since(startTime)
			totalOps := stats.totalOps.Load()
			opsPerSec := float64(totalOps) / elapsed.Seconds()

			fmt.Printf("[%6.1fs] Ops: %8d (%.1f ops/s) | R: %6d W: %6d D: %4d M: %4d L: %4d | Errors: %4d\n",
				elapsed.Seconds(),
				totalOps,
				opsPerSec,
				stats.reads.Load(),
				stats.writes.Load(),
				stats.deletes.Load(),
				stats.mkdirs.Load(),
				stats.lists.Load(),
				stats.errors.Load(),
			)
		}
	}
}

// printFinalReport prints the final statistics
func printFinalReport(stats *Stats, elapsed time.Duration) {
	totalOps := stats.totalOps.Load()
	reads := stats.reads.Load()
	writes := stats.writes.Load()
	deletes := stats.deletes.Load()
	mkdirs := stats.mkdirs.Load()
	lists := stats.lists.Load()
	errors := stats.errors.Load()
	bytesRead := stats.bytesRead.Load()
	bytesWritten := stats.bytesWritten.Load()

	opsPerSec := float64(totalOps) / elapsed.Seconds()
	readThroughput := float64(bytesRead) / elapsed.Seconds()
	writeThroughput := float64(bytesWritten) / elapsed.Seconds()

	fmt.Printf("\n")
	fmt.Printf("Final Results\n")
	fmt.Printf("=============\n")
	fmt.Printf("Duration:          %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Total Operations:  %d\n", totalOps)
	fmt.Printf("Operations/sec:    %.2f\n", opsPerSec)
	fmt.Printf("\n")
	fmt.Printf("Operation Breakdown:\n")
	fmt.Printf("  Reads:           %d (%.1f%%)\n", reads, percent(reads, totalOps))
	fmt.Printf("  Writes:          %d (%.1f%%)\n", writes, percent(writes, totalOps))
	fmt.Printf("  Deletes:         %d (%.1f%%)\n", deletes, percent(deletes, totalOps))
	fmt.Printf("  Mkdirs:          %d (%.1f%%)\n", mkdirs, percent(mkdirs, totalOps))
	fmt.Printf("  Lists:           %d (%.1f%%)\n", lists, percent(lists, totalOps))
	fmt.Printf("  Errors:          %d (%.1f%%)\n", errors, percent(errors, totalOps))
	fmt.Printf("\n")
	fmt.Printf("Throughput:\n")
	fmt.Printf("  Read:            %s/s (%d bytes total)\n", formatBytes(int64(readThroughput)), bytesRead)
	fmt.Printf("  Write:           %s/s (%d bytes total)\n", formatBytes(int64(writeThroughput)), bytesWritten)
	fmt.Printf("\n")

	if errors > 0 {
		fmt.Printf("⚠️  Test completed with %d errors (%.2f%% error rate)\n", errors, percent(errors, totalOps))
	} else {
		fmt.Printf("✅ Test completed successfully with no errors\n")
	}
}

// cleanup removes the test directory
func cleanup(dir string) {
	if dir == "" {
		return
	}
	if *verbose {
		fmt.Printf("\nCleaning up test directory: %s\n", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("Warning: failed to clean up test directory: %v", err)
	}
}

// scanExistingFiles walks the directory tree and collects file paths
func scanExistingFiles(root string, maxFiles int) []string {
	var files []string
	count := 0

	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if *verbose {
				log.Printf("Walk error: %v", err)
			}
			return nil // Continue on errors
		}

		// Skip directories
		if d.IsDir() {
			return nil
		}

		// Skip hidden files and common non-content files
		name := d.Name()
		if name[0] == '.' || name == "LICENSE" || name == "COPYING" {
			return nil
		}

		// Prefer source code files for realistic testing
		ext := filepath.Ext(name)
		if ext == ".go" || ext == ".py" || ext == ".js" || ext == ".ts" ||
			ext == ".java" || ext == ".c" || ext == ".h" || ext == ".cpp" ||
			ext == ".rs" || ext == ".rb" || ext == ".md" || ext == ".txt" ||
			ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".toml" {
			files = append(files, path)
			count++
		}

		if count >= maxFiles {
			return filepath.SkipAll
		}

		return nil
	})

	return files
}

// Helper functions
func randFloat() float64 {
	var b [8]byte
	rand.Read(b[:])
	return float64(b[0]) / 255.0
}

func randInt(max int) int {
	if max == 0 {
		return 0
	}
	var b [4]byte
	rand.Read(b[:])
	return int(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) % max
}

func percent(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100.0 / float64(total)
}

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
