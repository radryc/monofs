package router

import (
	"reflect"
	"testing"
)

func TestIntersectPartitionsNarrowsRequestScope(t *testing.T) {
	got := intersectPartitions([]string{"prod-eu", "prod-us", "staging"}, []string{"prod-us", "staging", "dev"})
	want := []string{"prod-us", "staging"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intersectPartitions() = %v, want %v", got, want)
	}
	if got := intersectPartitions(nil, []string{"prod-us"}); len(got) != 0 {
		t.Fatalf("empty request scope = %v, want empty", got)
	}
	if got := intersectPartitions([]string{"prod-us"}, nil); len(got) != 0 {
		t.Fatalf("empty router scope = %v, want empty", got)
	}
}
