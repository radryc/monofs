package logengine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxTraceDiscoveryTagKeys   = 128
	maxTraceDiscoveryTagValues = 64
)

func collectTraceDiscovery(spans []SpanRecord) ([]string, map[string][]string) {
	keys := map[string]struct{}{
		"service.name": {},
		"name":         {},
	}
	values := make(map[string]map[string]struct{})
	addKey := func(key string) {
		if len(keys) >= maxTraceDiscoveryTagKeys {
			return
		}
		keys[key] = struct{}{}
	}
	addValue := func(key, value string) {
		if key == "" || value == "" {
			return
		}
		if _, ok := keys[key]; !ok {
			addKey(key)
		}
		set := values[key]
		if set == nil {
			set = make(map[string]struct{})
			values[key] = set
		}
		if len(set) >= maxTraceDiscoveryTagValues {
			return
		}
		set[value] = struct{}{}
	}
	for _, span := range spans {
		addValue("service.name", span.Service)
		addValue("name", span.Name)
		if span.StatusCode != "" {
			addValue("status", span.StatusCode)
		}
		for key, value := range span.Attributes {
			key = strings.TrimSpace(key)
			if key == "trace_id" || key == "span_id" || key == "parent_span_id" {
				continue
			}
			addValue(key, value)
		}
	}
	outKeys := make([]string, 0, len(keys))
	for key := range keys {
		outKeys = append(outKeys, key)
	}
	sort.Strings(outKeys)
	outValues := make(map[string][]string, len(values))
	for key, set := range values {
		list := make([]string, 0, len(set))
		for value := range set {
			list = append(list, value)
		}
		sort.Strings(list)
		outValues[key] = list
	}
	return outKeys, outValues
}

// DiscoverTraceServices returns the services present in trace chunks that
// overlap the requested time range, using only chunk manifests.
func (q *QueryEngine) DiscoverTraceServices(ctx context.Context, from, to time.Time) ([]string, error) {
	manifests, err := q.traceDiscoveryManifests(ctx, from, to)
	if err != nil {
		return nil, err
	}
	values := make(map[string]struct{})
	for _, candidate := range manifests {
		for _, service := range candidate.manifest.Services {
			if service != "" {
				values[service] = struct{}{}
			}
		}
	}
	return sortedSet(values), nil
}

// DiscoverTraceTags returns tag names from trace chunk manifests. Older chunks
// without discovery metadata are scanned once as a compatibility fallback.
func (q *QueryEngine) DiscoverTraceTags(ctx context.Context, from, to time.Time) ([]string, error) {
	manifests, err := q.traceDiscoveryManifests(ctx, from, to)
	if err != nil {
		return nil, err
	}
	values := make(map[string]struct{})
	for _, candidate := range manifests {
		if candidate.manifest.TraceTagKeys != nil {
			for _, key := range candidate.manifest.TraceTagKeys {
				if key != "" {
					values[key] = struct{}{}
				}
			}
			continue
		}
		if err := q.scanTraceDiscovery(ctx, candidate.chunkID, func(span SpanRecord) {
			keys, _ := collectTraceDiscovery([]SpanRecord{span})
			for _, key := range keys {
				values[key] = struct{}{}
			}
		}); err != nil {
			if err == ErrGhostChunk {
				continue
			}
			return nil, err
		}
	}
	return sortedSet(values), nil
}

// DiscoverTraceTagValues returns values for one tag from chunk manifests.
func (q *QueryEngine) DiscoverTraceTagValues(ctx context.Context, from, to time.Time, tag string) ([]string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("trace discovery tag is required")
	}
	// Service names are already maintained as a first-class manifest field.
	// Avoid the tag-value path (and any legacy chunk fallback) for the most
	// common picker query.
	if tag == "service.name" || tag == "service" {
		return q.DiscoverTraceServices(ctx, from, to)
	}
	manifests, err := q.traceDiscoveryManifests(ctx, from, to)
	if err != nil {
		return nil, err
	}
	values := make(map[string]struct{})
	for _, candidate := range manifests {
		if candidate.manifest.TraceTagValues != nil {
			for _, value := range candidate.manifest.TraceTagValues[tag] {
				if value != "" {
					values[value] = struct{}{}
				}
			}
			continue
		}
		if err := q.scanTraceDiscovery(ctx, candidate.chunkID, func(span SpanRecord) {
			_, tagValues := collectTraceDiscovery([]SpanRecord{span})
			for _, value := range tagValues[tag] {
				values[value] = struct{}{}
			}
		}); err != nil {
			if err == ErrGhostChunk {
				continue
			}
			return nil, err
		}
	}
	return sortedSet(values), nil
}

func (q *QueryEngine) traceDiscoveryManifests(ctx context.Context, from, to time.Time) ([]candidateChunk, error) {
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/traces/")
	if err != nil {
		return nil, fmt.Errorf("failed to list trace chunks: %w", err)
	}
	remaining := make([]string, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if minT, maxT, ok := ParseChunkTimeRangeFromID(chunkID); ok {
			if !to.IsZero() && minT.After(to) {
				continue
			}
			if !from.IsZero() && maxT.Before(from) {
				continue
			}
		}
		remaining = append(remaining, chunkID)
	}
	q.prefetchManifests(ctx, SignalTraces, remaining)
	candidates := make([]candidateChunk, 0, len(remaining))
	for _, chunkID := range remaining {
		manifest, err := q.store.ReadManifest(ctx, SignalTraces, chunkID)
		if err != nil {
			if err == ErrGhostChunk {
				continue
			}
			return nil, err
		}
		if !to.IsZero() && manifest.MinTime.After(to) {
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			continue
		}
		candidates = append(candidates, candidateChunk{chunkID: chunkID, manifest: manifest, cutoffMax: manifest.MaxTime})
	}
	return candidates, nil
}

func (q *QueryEngine) scanTraceDiscovery(ctx context.Context, chunkID string, collect func(SpanRecord)) error {
	observer := beginQueryPathObservation(SignalTraces)
	defer observer.finish()
	path := filepath.Join("chunks", string(SignalTraces), chunkID, "data.parquet")
	return q.scanTraceParquet(ctx, path, "", "", time.Time{}, time.Time{}, observer, func(record SpanRecord) error {
		collect(record)
		return nil
	})
}

func sortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
