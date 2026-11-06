package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

// statsCacheTTL bounds how often the node queries ClickHouse system tables.
// Router health checks call GetNodeInfo frequently, so an uncached stats probe
// would turn into a steady stream of system-table scans.
const statsCacheTTL = 15 * time.Second

type clickHouseStatsCache struct {
	mu       sync.Mutex
	snapshot logengine.ClickHouseStats
	at       time.Time
}

// ClickHouseStats returns a cached snapshot of ClickHouse health, disk space,
// and table sizes. It never returns an error: a failed probe is reported in the
// snapshot so the UI can distinguish "unreachable" from "not configured".
func (s *TraceStore) ClickHouseStats(ctx context.Context) logengine.ClickHouseStats {
	s.stats.mu.Lock()
	if !s.stats.at.IsZero() && time.Since(s.stats.at) < statsCacheTTL {
		cached := s.stats.snapshot
		s.stats.mu.Unlock()
		return cached
	}
	snapshot := s.collectStats(ctx)
	s.stats.snapshot = snapshot
	s.stats.at = time.Now()
	s.stats.mu.Unlock()
	return snapshot
}

func (s *TraceStore) collectStats(ctx context.Context) logengine.ClickHouseStats {
	now := time.Now()
	snapshot := logengine.ClickHouseStats{
		Enabled:          true,
		URL:              s.displayURL(),
		Database:         s.cfg.Database,
		Tenant:           s.cfg.Tenant,
		RetentionSeconds: int64(s.cfg.Retention / time.Second),
		Measured:         now,
	}

	// Bound the probe independently of the heartbeat deadline. A down
	// ClickHouse must not stall the node's GetNodeInfo response.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = probeCtx

	version, err := s.queryVersion(ctx)
	if err != nil {
		snapshot.Healthy = false
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Healthy = true
	snapshot.Version = version

	disks, err := s.queryDisks(ctx)
	if err != nil {
		snapshot.Healthy = false
		snapshot.Error = fmt.Sprintf("query clickhouse disks: %v", err)
		return snapshot
	}
	snapshot.DiskTotalBytes = disks.TotalBytes
	snapshot.DiskFreeBytes = disks.FreeBytes
	if disks.TotalBytes >= disks.FreeBytes {
		snapshot.DiskUsedBytes = disks.TotalBytes - disks.FreeBytes
	}

	parts, err := s.queryParts(ctx)
	if err != nil {
		snapshot.Healthy = false
		snapshot.Error = fmt.Sprintf("query clickhouse parts: %v", err)
		return snapshot
	}
	for _, part := range parts {
		snapshot.TotalBytes += part.Bytes
		snapshot.TotalRows += part.Rows
		snapshot.Parts += part.Parts
		switch part.Name {
		case s.cfg.Table:
			snapshot.SpansBytes = part.Bytes
			snapshot.SpansRows = part.Rows
		case s.cfg.LogsTable:
			snapshot.LogsBytes = part.Bytes
			snapshot.LogsRows = part.Rows
		case s.cfg.MetricsTable:
			snapshot.MetricsBytes = part.Bytes
			snapshot.MetricsRows = part.Rows
		}
	}
	return snapshot
}

func (s *TraceStore) displayURL() string {
	parsed, err := url.Parse(s.baseURL)
	if err != nil {
		return s.baseURL
	}
	parsed.User = nil
	return parsed.String()
}

func (s *TraceStore) queryVersion(ctx context.Context) (string, error) {
	rows, err := s.queryStats(ctx, "SELECT version() AS version FORMAT JSONEachRow", nil)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("clickhouse returned no version row")
	}
	return rows[0].Version, nil
}

func (s *TraceStore) queryDisks(ctx context.Context) (statsRow, error) {
	rows, err := s.queryStats(ctx, "SELECT sum(total_space) AS total_bytes, sum(free_space) AS free_bytes FROM system.disks FORMAT JSONEachRow", nil)
	if err != nil {
		return statsRow{}, err
	}
	if len(rows) == 0 {
		return statsRow{}, fmt.Errorf("clickhouse returned no disk row")
	}
	return rows[0], nil
}

func (s *TraceStore) queryParts(ctx context.Context) ([]statsRow, error) {
	params := url.Values{}
	params.Set("param_db", s.cfg.Database)
	return s.queryStats(ctx,
		"SELECT table AS name, sum(rows) AS rows, sum(bytes_on_disk) AS bytes, count() AS parts "+
			"FROM system.parts WHERE active AND database = {db:String} GROUP BY table FORMAT JSONEachRow",
		params)
}

type statsRow struct {
	Version    string `json:"version"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	Name       string `json:"name"`
	Rows       uint64 `json:"rows"`
	Bytes      uint64 `json:"bytes"`
	Parts      uint64 `json:"parts"`
}

func (s *TraceStore) queryStats(ctx context.Context, statement string, params url.Values) ([]statsRow, error) {
	response, err := s.do(ctx, statement, params, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	rows := make([]statsRow, 0, 8)
	decoder := json.NewDecoder(response.Body)
	for {
		var row statsRow
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode clickhouse stats row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}
