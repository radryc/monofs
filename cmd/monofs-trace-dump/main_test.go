package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseTimeSpecRelativeNowSupportsUppercaseUnits(t *testing.T) {
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	got, err := parseTimeSpec("now-2H30M", now, time.UTC)
	if err != nil {
		t.Fatalf("parseTimeSpec() error = %v", err)
	}
	want := now.Add(-(2*time.Hour + 30*time.Minute))
	if !got.Equal(want) {
		t.Fatalf("parseTimeSpec() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestResolveWindowRangeAbsoluteTimestamps(t *testing.T) {
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	window, err := resolveWindow("", "", "2026-05-01 12:00:00 to 2026-05-02 12:00:01", now, time.UTC)
	if err != nil {
		t.Fatalf("resolveWindow() error = %v", err)
	}
	wantFrom := time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, time.May, 2, 12, 0, 1, 0, time.UTC)
	if !window.from.Equal(wantFrom) {
		t.Fatalf("window.from = %s, want %s", window.from.Format(time.RFC3339Nano), wantFrom.Format(time.RFC3339Nano))
	}
	if !window.to.Equal(wantTo) {
		t.Fatalf("window.to = %s, want %s", window.to.Format(time.RFC3339Nano), wantTo.Format(time.RFC3339Nano))
	}
}

func TestResolveWindowDefaultsToNowWhenToMissing(t *testing.T) {
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	window, err := resolveWindow("now-15m", "", "", now, time.UTC)
	if err != nil {
		t.Fatalf("resolveWindow() error = %v", err)
	}
	wantFrom := now.Add(-15 * time.Minute)
	if !window.from.Equal(wantFrom) {
		t.Fatalf("window.from = %s, want %s", window.from.Format(time.RFC3339Nano), wantFrom.Format(time.RFC3339Nano))
	}
	if !window.to.Equal(now) {
		t.Fatalf("window.to = %s, want %s", window.to.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
}

func TestParseFlagsAcceptsOutputPath(t *testing.T) {
	cfg, err := parseFlags([]string{"--from", "now-5m", "--output", "spans.json"})
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if cfg.output != "spans.json" {
		t.Fatalf("cfg.output = %q, want spans.json", cfg.output)
	}
}

func TestResolveOutputCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.json")
	writer, file, err := resolveOutput(path)
	if err != nil {
		t.Fatalf("resolveOutput() error = %v", err)
	}
	if file == nil {
		t.Fatal("resolveOutput() file = nil, want created file")
	}
	if writer != file {
		t.Fatal("resolveOutput() writer should be the created file")
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file.Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("os.Stat(%q) error = %v", path, err)
	}
}
