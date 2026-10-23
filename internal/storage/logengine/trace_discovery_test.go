package logengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTraceDiscoveryUsesChunkManifests(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_trace_discovery_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{LocalCacheDir: filepath.Join(tmpDir, "cache"), ChunkDuration: 5 * time.Minute})
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	if err := engine.IngestTraces(context.Background(), "chunk-one", []SpanRecord{
		{Timestamp: base.Add(-10 * time.Minute), EndTime: base.Add(-10*time.Minute + time.Second), TraceID: "t1", SpanID: "s1", Service: "api", Name: "GET /orders", StatusCode: "OK", Attributes: map[string]string{"http.method": "GET", "guardian.partition": "div-a-orders"}},
		{Timestamp: base.Add(-9 * time.Minute), EndTime: base.Add(-9*time.Minute + time.Second), TraceID: "t2", SpanID: "s2", Service: "worker", Name: "process", Attributes: map[string]string{"http.method": "POST", "guardian.partition": "div-a-orders"}},
	}); err != nil {
		t.Fatalf("IngestTraces: %v", err)
	}
	if err := engine.IngestTraces(context.Background(), "chunk-two", []SpanRecord{
		{Timestamp: base.Add(-2 * time.Hour), EndTime: base.Add(-2*time.Hour + time.Second), TraceID: "t3", SpanID: "s3", Service: "old-service", Name: "old"},
	}); err != nil {
		t.Fatalf("IngestTraces(old): %v", err)
	}

	from := base.Add(-30 * time.Minute)
	to := base.Add(time.Minute)
	services, err := engine.DiscoverTraceServices(context.Background(), from, to)
	if err != nil {
		t.Fatalf("DiscoverTraceServices: %v", err)
	}
	if len(services) != 2 || services[0] != "api" || services[1] != "worker" {
		t.Fatalf("services = %#v", services)
	}
	tags, err := engine.DiscoverTraceTags(context.Background(), from, to)
	if err != nil {
		t.Fatalf("DiscoverTraceTags: %v", err)
	}
	for _, want := range []string{"service.name", "name", "status", "http.method", "guardian.partition"} {
		if !containsString(tags, want) {
			t.Fatalf("tags = %#v, missing %q", tags, want)
		}
	}
	values, err := engine.DiscoverTraceTagValues(context.Background(), from, to, "service.name")
	if err != nil {
		t.Fatalf("DiscoverTraceTagValues: %v", err)
	}
	if len(values) != 2 || values[0] != "api" || values[1] != "worker" {
		t.Fatalf("service values = %#v", values)
	}
	methods, err := engine.DiscoverTraceTagValues(context.Background(), from, to, "http.method")
	if err != nil {
		t.Fatalf("DiscoverTraceTagValues(http.method): %v", err)
	}
	if len(methods) != 2 || methods[0] != "GET" || methods[1] != "POST" {
		t.Fatalf("http methods = %#v", methods)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
