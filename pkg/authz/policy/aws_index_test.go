package policy

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPartitionForAWSPrecedence(t *testing.T) {
	p := New(Bundle{AWSBindings: []AWSBinding{
		{Partition: "region-only", Region: "us-east-1"},
		{Partition: "account-only", Account: "111111111111"},
		{Partition: "exact", Account: "111111111111", Region: "us-east-1"},
		{Partition: "wildcard"},
	}})
	cases := []struct {
		account string
		region  string
		want    string
	}{
		{"111111111111", "us-east-1", "exact"},
		{"111111111111", "eu-west-1", "account-only"},
		{"222222222222", "us-east-1", "region-only"},
		{"222222222222", "eu-west-1", "wildcard"},
	}
	for _, tc := range cases {
		got, ok := p.PartitionForAWS(tc.account, tc.region)
		if !ok || got != tc.want {
			t.Fatalf("PartitionForAWS(%s,%s) = %q,%v; want %q,true", tc.account, tc.region, got, ok, tc.want)
		}
	}
	if _, ok := p.PartitionForAWS("", ""); ok {
		t.Fatal("empty account/region must not resolve")
	}
}

func TestPartitionForAWS50kLookupBudget(t *testing.T) {
	if os.Getenv("MONOFS_LOADTEST") != "1" && os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set MONOFS_LOADTEST=1 to run the 50k binding lookup budget test")
	}
	const count = 50000
	bindings := make([]AWSBinding, 0, count)
	for i := 0; i < count; i++ {
		bindings = append(bindings, AWSBinding{
			Partition: fmt.Sprintf("part-%05d", i),
			Account:   fmt.Sprintf("%012d", i),
			Region:    "us-east-1",
		})
	}
	p := New(Bundle{AWSBindings: bindings})
	start := time.Now()
	for i := 0; i < 1_000_000; i++ {
		account := fmt.Sprintf("%012d", i%count)
		partition, ok := p.PartitionForAWS(account, "us-east-1")
		if !ok || partition == "" {
			t.Fatalf("lookup %d failed", i)
		}
	}
	elapsed := time.Since(start)
	t.Logf("50k bindings x 1M lookups = %s (%.0f ns/lookup)", elapsed, float64(elapsed.Nanoseconds())/1_000_000)
	if elapsed > 2*time.Second {
		t.Fatalf("1M lookups took %s, budget is 2s", elapsed)
	}
}

func TestCompile50kCatalogBindings(t *testing.T) {
	if os.Getenv("MONOFS_LOADTEST") != "1" && os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set MONOFS_LOADTEST=1 to run the 50k policy compile budget test")
	}
	const count = 50000
	partitions := make([]Partition, 0, count)
	for i := 0; i < count; i++ {
		partitions = append(partitions, Partition{
			Name: fmt.Sprintf("part-%05d", i),
			Labels: map[string]string{
				"aws.account": fmt.Sprintf("%012d", i),
				"aws.region":  "us-east-1",
			},
		})
	}
	file, err := ParseFile([]byte(`version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
`))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	start := time.Now()
	bundle, _, err := Compile(file, &Catalog{Partitions: partitions}, time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	elapsed := time.Since(start)
	if len(bundle.AWSBindings) != count {
		t.Fatalf("derived bindings = %d, want %d", len(bundle.AWSBindings), count)
	}
	p := New(*bundle)
	if partition, ok := p.PartitionForAWS("000000000042", "us-east-1"); !ok || partition != "part-00042" {
		t.Fatalf("derived lookup = %q,%v", partition, ok)
	}
	t.Logf("50k catalog bindings compiled in %s", elapsed)
	if elapsed > 5*time.Second {
		t.Fatalf("compile took %s, budget is 5s", elapsed)
	}
}

func BenchmarkPartitionForAWS50k(b *testing.B) {
	const count = 50000
	bindings := make([]AWSBinding, 0, count)
	for i := 0; i < count; i++ {
		bindings = append(bindings, AWSBinding{
			Partition: fmt.Sprintf("part-%05d", i),
			Account:   fmt.Sprintf("%012d", i),
			Region:    "us-east-1",
		})
	}
	p := New(Bundle{AWSBindings: bindings})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.PartitionForAWS(fmt.Sprintf("%012d", i%count), "us-east-1")
	}
}
