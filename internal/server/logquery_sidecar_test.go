package server

import (
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestForEachSidecarItem(t *testing.T) {
	var got [][]byte
	item := &pb.QueryResultItem{ItemsJson: [][]byte{[]byte("a"), []byte("b")}}
	if err := forEachSidecarItem(item, func(raw []byte) error {
		got = append(got, append([]byte(nil), raw...))
		return nil
	}); err != nil {
		t.Fatalf("forEachSidecarItem() error = %v", err)
	}
	if len(got) != 2 || string(got[0]) != "a" || string(got[1]) != "b" {
		t.Fatalf("items = %q, want [a b]", got)
	}

	got = nil
	if err := forEachSidecarItem(&pb.QueryResultItem{ItemJson: []byte("legacy")}, func(raw []byte) error {
		got = append(got, append([]byte(nil), raw...))
		return nil
	}); err != nil {
		t.Fatalf("legacy forEachSidecarItem() error = %v", err)
	}
	if len(got) != 1 || string(got[0]) != "legacy" {
		t.Fatalf("legacy items = %q", got)
	}
}

func TestShouldFallbackSidecar(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted} {
		if !shouldFallbackSidecar(status.Error(code, "transient")) {
			t.Fatalf("shouldFallbackSidecar(%s) = false, want true", code)
		}
	}
	for _, code := range []codes.Code{codes.InvalidArgument, codes.Internal, codes.PermissionDenied} {
		if shouldFallbackSidecar(status.Error(code, "query error")) {
			t.Fatalf("shouldFallbackSidecar(%s) = true, want false", code)
		}
	}
}

func TestMetricRequestRoundTrip(t *testing.T) {
	from := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	req := metricRequest(logengine.MetricQuery{
		MetricName: "requests_total",
		Service:    "guardian",
		LabelMatchers: []logengine.MetricLabelMatcher{{
			Name:  "env",
			Value: "prod",
			Type:  logengine.MetricMatchNotRegexp,
		}},
	}, from, to)
	if req.GetMetricName() != "requests_total" || req.GetService() != "guardian" {
		t.Fatalf("metric request = %+v", req)
	}
	if req.GetFromUnixNano() != from.UnixNano() || req.GetToUnixNano() != to.UnixNano() {
		t.Fatalf("metric request times = %d..%d", req.GetFromUnixNano(), req.GetToUnixNano())
	}
	if len(req.GetLabelMatchers()) != 1 || req.GetLabelMatchers()[0].GetType() != pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_REGEXP {
		t.Fatalf("metric matchers = %+v", req.GetLabelMatchers())
	}
}
