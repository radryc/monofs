package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultTimeout = 30 * time.Second

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

type config struct {
	addr      string
	api       string
	traceID   string
	service   string
	from      string
	to        string
	rangeSpec string
	format    string
	output    string
	timezone  string
	timeout   time.Duration
	limit     int
	showVer   bool
}

type timeWindow struct {
	from time.Time
	to   time.Time
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if cfg.showVer {
		fmt.Printf("monofs-trace-dump %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		return
	}

	loc, err := parseLocation(cfg.timezone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	now := time.Now().In(loc)
	window, err := resolveWindow(cfg.from, cfg.to, cfg.rangeSpec, now, loc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	spans, err := dumpTraces(ctx, cfg, window)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	out, file, err := resolveOutput(cfg.output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := writeOutput(out, spans, cfg.format); err != nil {
		if file != nil {
			_ = file.Close()
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if file != nil {
		if err := file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: close output file: %v\n", err)
			os.Exit(1)
		}
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("monofs-trace-dump", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := config{}
	fs.StringVar(&cfg.addr, "addr", "localhost:9090", "MonoFS gRPC address")
	fs.StringVar(&cfg.api, "api", "router", "API surface: router or server")
	fs.StringVar(&cfg.traceID, "trace-id", "", "Optional trace_id filter")
	fs.StringVar(&cfg.service, "service", "", "Optional service filter")
	fs.StringVar(&cfg.from, "from", "", "Range start, e.g. now-2h or 2026-05-01 12:00:00")
	fs.StringVar(&cfg.to, "to", "", "Range end, default is now when --from is set")
	fs.StringVar(&cfg.rangeSpec, "range", "", "Combined range, e.g. '2026-05-01 12:00:00 to 2026-05-02 12:00:01'")
	fs.StringVar(&cfg.format, "format", "json-pretty", "Output format: json, json-pretty, or table")
	fs.StringVar(&cfg.output, "output", "", "Write output to a file instead of stdout; use - for stdout")
	fs.StringVar(&cfg.timezone, "timezone", "UTC", "Timezone for absolute timestamps without an offset, e.g. UTC or Europe/Warsaw")
	fs.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "Request timeout")
	fs.IntVar(&cfg.limit, "limit", 0, "Maximum spans to return, 0 means no limit")
	fs.BoolVar(&cfg.showVer, "version", false, "Show version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `MonoFS Trace Dump

Usage:
  monofs-trace-dump [flags]

Examples:
  monofs-trace-dump --addr localhost:9090 --api router --from now-2H
  monofs-trace-dump --addr localhost:9090 --api router --range "2026-05-01 12:00:00 to 2026-05-02 12:00:01"
  monofs-trace-dump --addr localhost:9000 --api server --service monofs-server --from now-30m --format table
	monofs-trace-dump --addr localhost:9090 --api router --from now-2H --output spans.json

Time formats:
  - Relative: now, now-2h, now-15m, now+30s
  - Absolute with zone: 2026-05-01T12:00:00Z, 2026-05-01T12:00:00+02:00
  - Absolute without zone: 2026-05-01 12:00:00, 2026-05-01T12:00:00

When absolute timestamps omit a timezone, --timezone is used and defaults to UTC.

`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.limit < 0 {
		return cfg, errors.New("--limit must be >= 0")
	}
	if cfg.timeout <= 0 {
		return cfg, errors.New("--timeout must be > 0")
	}
	return cfg, nil
}

func resolveOutput(path string) (io.Writer, *os.File, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == "-" {
		return os.Stdout, nil, nil
	}
	file, err := os.Create(trimmed)
	if err != nil {
		return nil, nil, fmt.Errorf("open output file %q: %w", trimmed, err)
	}
	return file, file, nil
}

func parseLocation(name string) (*time.Location, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || strings.EqualFold(trimmed, "utc") {
		return time.UTC, nil
	}
	if strings.EqualFold(trimmed, "local") {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(trimmed)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", name, err)
	}
	return loc, nil
}

func resolveWindow(fromSpec, toSpec, rangeSpec string, now time.Time, loc *time.Location) (timeWindow, error) {
	if strings.TrimSpace(rangeSpec) != "" {
		if strings.TrimSpace(fromSpec) != "" || strings.TrimSpace(toSpec) != "" {
			return timeWindow{}, errors.New("use either --range or --from/--to, not both")
		}
		startSpec, endSpec, err := splitRangeSpec(rangeSpec)
		if err != nil {
			return timeWindow{}, err
		}
		from, err := parseTimeSpec(startSpec, now, loc)
		if err != nil {
			return timeWindow{}, fmt.Errorf("parse range start: %w", err)
		}
		to, err := parseTimeSpec(endSpec, now, loc)
		if err != nil {
			return timeWindow{}, fmt.Errorf("parse range end: %w", err)
		}
		if from.After(to) {
			return timeWindow{}, errors.New("range start must be before range end")
		}
		return timeWindow{from: from, to: to}, nil
	}

	if strings.TrimSpace(fromSpec) == "" {
		return timeWindow{}, errors.New("either --from or --range is required")
	}
	from, err := parseTimeSpec(fromSpec, now, loc)
	if err != nil {
		return timeWindow{}, fmt.Errorf("parse --from: %w", err)
	}
	to := now
	if strings.TrimSpace(toSpec) != "" {
		to, err = parseTimeSpec(toSpec, now, loc)
		if err != nil {
			return timeWindow{}, fmt.Errorf("parse --to: %w", err)
		}
	}
	if from.After(to) {
		return timeWindow{}, errors.New("--from must be before --to")
	}
	return timeWindow{from: from, to: to}, nil
}

func splitRangeSpec(spec string) (string, string, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return "", "", errors.New("empty range")
	}
	if idx := strings.Index(strings.ToLower(trimmed), " to "); idx >= 0 {
		left := strings.TrimSpace(trimmed[:idx])
		right := strings.TrimSpace(trimmed[idx+4:])
		if left == "" || right == "" {
			return "", "", errors.New("range must contain both start and end")
		}
		return left, right, nil
	}
	parts := strings.SplitN(trimmed, ",", 2)
	if len(parts) == 2 {
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		if left == "" || right == "" {
			return "", "", errors.New("range must contain both start and end")
		}
		return left, right, nil
	}
	return "", "", errors.New("range must be either 'start to end' or 'start,end'")
}

func parseTimeSpec(spec string, now time.Time, loc *time.Location) (time.Time, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return time.Time{}, errors.New("empty time spec")
	}
	if strings.EqualFold(trimmed, "now") {
		return now, nil
	}

	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "now") {
		offsetSpec := strings.TrimSpace(lower[3:])
		if offsetSpec == "" {
			return now, nil
		}
		delta, err := time.ParseDuration(offsetSpec)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse relative duration %q: %w", spec, err)
		}
		return now.Add(delta), nil
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed, nil
		}
	}

	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	} {
		if parsed, err := time.ParseInLocation(layout, trimmed, loc); err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, fmt.Errorf("unsupported time format %q", spec)
}

func dumpTraces(ctx context.Context, cfg config, window timeWindow) ([]logengine.SpanRecord, error) {
	conn, err := grpc.DialContext(ctx, cfg.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.addr, err)
	}
	defer conn.Close()

	req := &pb.QueryTracesRequest{
		TraceId:      cfg.traceID,
		Service:      cfg.service,
		FromUnixNano: window.from.UTC().UnixNano(),
		ToUnixNano:   window.to.UTC().UnixNano(),
		Limit:        int32(cfg.limit),
	}

	var resp *pb.QueryTracesResponse
	switch strings.ToLower(strings.TrimSpace(cfg.api)) {
	case "router":
		resp, err = pb.NewMonoFSRouterClient(conn).QueryTraces(ctx, req)
	case "server", "monofs", "node":
		resp, err = pb.NewMonoFSClient(conn).QueryTraces(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported --api %q, want router or server", cfg.api)
	}
	if err != nil {
		return nil, fmt.Errorf("query traces: %w", err)
	}

	spans := make([]logengine.SpanRecord, 0)
	if len(resp.GetResultsJson()) == 0 {
		return spans, nil
	}
	if err := json.Unmarshal(resp.GetResultsJson(), &spans); err != nil {
		return nil, fmt.Errorf("decode trace response: %w", err)
	}
	sortSpans(spans)
	return spans, nil
}

func sortSpans(spans []logengine.SpanRecord) {
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].Timestamp.Equal(spans[j].Timestamp) {
			return spans[i].Timestamp.Before(spans[j].Timestamp)
		}
		if !spans[i].EndTime.Equal(spans[j].EndTime) {
			return spans[i].EndTime.Before(spans[j].EndTime)
		}
		if spans[i].TraceID != spans[j].TraceID {
			return spans[i].TraceID < spans[j].TraceID
		}
		return spans[i].SpanID < spans[j].SpanID
	})
}

func writeOutput(file io.Writer, spans []logengine.SpanRecord, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		enc := json.NewEncoder(file)
		return enc.Encode(spans)
	case "json-pretty", "pretty", "":
		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		return enc.Encode(spans)
	case "table":
		w := tabwriter.NewWriter(file, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "START\tEND\tSERVICE\tTRACE_ID\tSPAN_ID\tPARENT_SPAN_ID\tSTATUS\tNAME"); err != nil {
			return err
		}
		for _, span := range spans {
			if _, err := fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				span.Timestamp.UTC().Format(time.RFC3339Nano),
				span.EndTime.UTC().Format(time.RFC3339Nano),
				span.Service,
				span.TraceID,
				span.SpanID,
				span.ParentSpanID,
				span.StatusCode,
				span.Name,
			); err != nil {
				return err
			}
		}
		return w.Flush()
	default:
		return fmt.Errorf("unsupported --format %q, want json, json-pretty, or table", format)
	}
}
