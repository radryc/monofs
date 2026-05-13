package logquery

import "testing"

func TestParseSelectorAndLineFilter(t *testing.T) {
	parsed, err := Parse(`{service="payment"} |= "connection timeout"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := parsed.ServiceEquals(); got != "payment" {
		t.Fatalf("ServiceEquals() = %q, want %q", got, "payment")
	}
	if len(parsed.LineFilters) != 1 {
		t.Fatalf("len(LineFilters) = %d, want 1", len(parsed.LineFilters))
	}
	if got := parsed.LineFilters[0].Value; got != "connection timeout" {
		t.Fatalf("LineFilters[0].Value = %q, want %q", got, "connection timeout")
	}
}

func TestParseSkipsUnknownPipelineStages(t *testing.T) {
	parsed, err := Parse(`{service="payment"} | json |= "timeout"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := parsed.ServiceEquals(); got != "payment" {
		t.Fatalf("ServiceEquals() = %q, want %q", got, "payment")
	}
	if len(parsed.LineFilters) != 1 || parsed.LineFilters[0].Value != "timeout" {
		t.Fatalf("LineFilters = %#v, want one timeout filter", parsed.LineFilters)
	}
}

func TestParseMultipleLineFilters(t *testing.T) {
	parsed, err := Parse(`{service="payment"} |= "foo" != "bar" |~ "baz.*" !~ "qux"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.LineFilters) != 4 {
		t.Fatalf("len(LineFilters) = %d, want 4", len(parsed.LineFilters))
	}
	wantOps := []string{"|=", "!=", "|~", "!~"}
	for i, want := range wantOps {
		if got := parsed.LineFilters[i].Op; got != want {
			t.Fatalf("LineFilters[%d].Op = %q, want %q", i, got, want)
		}
	}
}

func TestParseMultipleMatchers(t *testing.T) {
	parsed, err := Parse(`{service=~"pay.*", level!="info", trace_id="abc"} |= "timeout"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Matchers) != 3 {
		t.Fatalf("len(Matchers) = %d, want 3", len(parsed.Matchers))
	}
	wantOps := []string{"=~", "!=", "="}
	for i, want := range wantOps {
		if got := parsed.Matchers[i].Op; got != want {
			t.Fatalf("Matchers[%d].Op = %q, want %q", i, got, want)
		}
	}
}

func TestPositiveLineContainsFilters(t *testing.T) {
	parsed, err := Parse(`{service="payment"} |= "foo" != "bar" |= "baz"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got := parsed.PositiveLineContainsFilters()
	if len(got) != 2 || got[0] != "foo" || got[1] != "baz" {
		t.Fatalf("PositiveLineContainsFilters() = %#v, want []string{\"foo\", \"baz\"}", got)
	}
}

func TestParseRejectsUnsupportedTrailingContent(t *testing.T) {
	if _, err := Parse(`{service="payment"} timeout`); err == nil {
		t.Fatalf("Parse() error = nil, want non-nil")
	}
}
