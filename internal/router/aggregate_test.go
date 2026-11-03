package router

import (
	"encoding/json"
	"testing"

	"github.com/radryc/monofs/internal/storage/logengine"
)

func TestDecodeAndMergeAggregateRows(t *testing.T) {
	left := []byte(`[{"group":{"service":"guardian"},"values":{"count":2,"sum_value":7,"min_ts":100,"max_ts":200}}]`)
	right := []byte(`[{"group":{"service":"guardian"},"values":{"count":3,"sum_value":8,"min_ts":50,"max_ts":300}}]`)
	specs := aggregateSpecByAlias{
		"count":     {Op: logengine.AggregateCount, Alias: "count"},
		"sum_value": {Op: logengine.AggregateSum, Field: "value", Alias: "sum_value"},
		"min_ts":    {Op: logengine.AggregateMin, Field: "timestamp", Alias: "min_ts"},
		"max_ts":    {Op: logengine.AggregateMax, Field: "timestamp", Alias: "max_ts"},
	}
	merged := make(map[string]*aggregateWireRow)
	for _, data := range [][]byte{left, right} {
		rows, err := decodeAggregateRows(data)
		if err != nil {
			t.Fatalf("decodeAggregateRows() error = %v", err)
		}
		for _, row := range rows {
			key := aggregateWireKey(row.Group, []string{"service"})
			target := merged[key]
			if target == nil {
				target = &aggregateWireRow{Group: row.Group, Values: make(map[string]json.Number)}
				merged[key] = target
			}
			for alias, value := range row.Values {
				spec := specs[alias]
				target.Values[alias] = mergeAggregateNumber(target.Values[alias], value, spec.Op)
			}
		}
	}
	row := merged["service=guardian\x00"]
	if row == nil {
		t.Fatal("missing merged row")
	}
	if got := row.Values["count"].String(); got != "5" {
		t.Fatalf("count = %s, want 5", got)
	}
	if got := row.Values["sum_value"].String(); got != "15" {
		t.Fatalf("sum = %s, want 15", got)
	}
	if got := row.Values["min_ts"].String(); got != "50" {
		t.Fatalf("min = %s, want 50", got)
	}
	if got := row.Values["max_ts"].String(); got != "300" {
		t.Fatalf("max = %s, want 300", got)
	}
}
