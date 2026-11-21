package sqlfederation

import (
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

func TestBuildCatalogUsesDeclaredMembers(t *testing.T) {
	nodes := []Node{{ID: "node-a"}, {ID: "node-b"}, {ID: "node-c"}}
	cat, err := BuildCatalog("logs", nodes, 5)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if len(cat.Endpoints) != 3 {
		t.Fatalf("endpoints = %d, want 3", len(cat.Endpoints))
	}
	group := cat.Groups["monofs-local"]
	if group == nil || group.Members != 5 {
		t.Fatalf("group = %#v, want members=5", group)
	}
}

func TestDecodePartialTypes(t *testing.T) {
	payload := `{
		"columns":["k0","p0","p1","p2","p3","p4"],
		"types":["VARCHAR","BIGINT","DOUBLE","DECIMAL(38,9)","TIMESTAMP","BOOLEAN"],
		"rows":[["api",9007199254740993,1.5,"5.000000000","2026-11-01T00:00:00Z",true]],
		"truncated":false
	}`
	stream, err := decodePartial("node-a", payload, []string{"k0", "p0", "p1", "p2", "p3", "p4"})
	if err != nil {
		t.Fatalf("decodePartial: %v", err)
	}
	if len(stream.Rows) != 1 {
		t.Fatalf("rows = %d", len(stream.Rows))
	}
	row := stream.Rows[0]
	if row[0].AsString() != "api" {
		t.Fatalf("k0 = %q", row[0].AsString())
	}
	if row[1].Int() != 9007199254740993 {
		t.Fatalf("int64 precision lost: %d", row[1].Int())
	}
	if row[2].Float64() != 1.5 {
		t.Fatalf("p1 = %v", row[2].Float64())
	}
	if row[3].AsString() != "5.000000000" {
		t.Fatalf("decimal = %s", row[3].AsString())
	}
	if !row[4].T.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %v", row[4].T)
	}
	if !row[5].B {
		t.Fatalf("bool = %#v", row[5])
	}
}

func TestDecodePartialRejectsTruncation(t *testing.T) {
	payload := `{"columns":["p0"],"types":["BIGINT"],"rows":[[1]],"truncated":true}`
	if _, err := decodePartial("node-a", payload, []string{"p0"}); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestConvertSelector(t *testing.T) {
	in := &pb.BackendSelector{
		Include:         []*pb.BackendMatcher{{Ids: []string{"node-0*"}}},
		Exclude:         []*pb.BackendMatcher{{Tags: map[string]string{"pool": "legacy"}}},
		TreatAsReplica:  []*pb.BackendMatcher{{Ids: []string{"node-09"}}},
		IncludeReplicas: true,
		Max:             7,
		MinCoverage:     0.9,
	}
	out := ConvertSelector(in)
	if len(out.Include) != 1 || out.Include[0].IDs[0] != "node-0*" {
		t.Fatalf("include = %#v", out.Include)
	}
	if out.Exclude[0].Tags["pool"] != "legacy" {
		t.Fatalf("exclude = %#v", out.Exclude)
	}
	if !out.IncludeReplicas || out.Max != 7 || out.MinCoverage != 0.9 {
		t.Fatalf("selector = %#v", out)
	}
}

var _ = sqlag.RoleShard
