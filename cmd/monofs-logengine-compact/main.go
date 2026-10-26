// Command monofs-logengine-compact backfills compaction for existing telemetry
// chunks. The server compacts recent windows in the background; use this command
// for older data after enabling time-bucketed storage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

func main() {
	dir := flag.String("dir", "", "Logengine data directory (required)")
	signalName := flag.String("signal", "all", "Signal to compact: logs, metrics, traces, or all")
	fromRaw := flag.String("from", "", "Start time in RFC3339 (required)")
	toRaw := flag.String("to", "", "End time in RFC3339 (required)")
	window := flag.Duration("window", 0, "Compaction window; 0 uses 1h for logs, 30m for traces, 15m for metrics")
	flag.Parse()

	if *dir == "" || *fromRaw == "" || *toRaw == "" {
		fmt.Fprintln(os.Stderr, "--dir, --from, and --to are required")
		flag.Usage()
		os.Exit(2)
	}
	from, err := time.Parse(time.RFC3339, *fromRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --from: %v\n", err)
		os.Exit(2)
	}
	to, err := time.Parse(time.RFC3339, *toRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --to: %v\n", err)
		os.Exit(2)
	}
	if !to.After(from) {
		fmt.Fprintln(os.Stderr, "--to must be after --from")
		os.Exit(2)
	}

	signals, err := selectSignals(*signalName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	engine := logengine.New(logengine.NewMockS3Store(*dir), logengine.Config{
		LocalCacheDir: *dir,
		ChunkDuration: 5 * time.Minute,
	})
	defer engine.Close()

	ctx := context.Background()
	for _, signal := range signals {
		step := *window
		if step <= 0 {
			step = defaultWindow(signal)
		}
		for start := from; start.Before(to); start = start.Add(step) {
			end := start.Add(step)
			if end.After(to) {
				end = to
			}
			result, err := engine.CompactRange(ctx, signal, start, end)
			if err != nil {
				fmt.Fprintf(os.Stderr, "compact %s %s..%s: %v\n", signal, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
				os.Exit(1)
			}
			if result.OutputChunks > 0 || result.DeletedChunks > 0 {
				fmt.Printf("%s %s..%s inputs=%d outputs=%d records=%d deleted=%d\n",
					signal, start.Format(time.RFC3339), end.Format(time.RFC3339),
					result.InputChunks, result.OutputChunks, result.Records, result.DeletedChunks)
			}
		}
	}
}

func selectSignals(name string) ([]logengine.Signal, error) {
	switch name {
	case "logs":
		return []logengine.Signal{logengine.SignalLogs}, nil
	case "metrics":
		return []logengine.Signal{logengine.SignalMetrics}, nil
	case "traces":
		return []logengine.Signal{logengine.SignalTraces}, nil
	case "all":
		return []logengine.Signal{logengine.SignalLogs, logengine.SignalMetrics, logengine.SignalTraces}, nil
	default:
		return nil, fmt.Errorf("unknown signal %q", name)
	}
}

func defaultWindow(signal logengine.Signal) time.Duration {
	switch signal {
	case logengine.SignalMetrics:
		return 15 * time.Minute
	case logengine.SignalTraces:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}
