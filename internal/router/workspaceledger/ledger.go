package workspaceledger

import (
	"encoding/json"
	"sync"

	pb "github.com/radryc/monofs/api/proto"
)

type WALWriter interface {
	InsertLedger(data []byte) error
}

type Ledger struct {
	mu  sync.RWMutex
	wal WALWriter

	commits   []*pb.LocalCommit
	outcomes  []*pb.PushOutcome
	refreshes []*pb.RefreshEvent
}

func New() *Ledger {
	return &Ledger{}
}

func NewWithWAL(wal WALWriter) *Ledger {
	return &Ledger{wal: wal}
}

type ledgerRecord struct {
	Table string          `json:"table"`
	Data  json.RawMessage `json:"data"`
}

func (l *Ledger) InsertCommit(c *pb.LocalCommit) {
	data, _ := json.Marshal(ledgerRecord{Table: "local_commits", Data: mustMarshal(c)})
	if l.wal != nil {
		_ = l.wal.InsertLedger(data)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.insertCommitLocked(c)
}

func (l *Ledger) InsertPushOutcome(o *pb.PushOutcome) {
	data, _ := json.Marshal(ledgerRecord{Table: "push_outcomes", Data: mustMarshal(o)})
	if l.wal != nil {
		_ = l.wal.InsertLedger(data)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.insertOutcomeLocked(o)
}

func (l *Ledger) InsertRefreshEvent(r *pb.RefreshEvent) {
	data, _ := json.Marshal(ledgerRecord{Table: "refresh_events", Data: mustMarshal(r)})
	if l.wal != nil {
		_ = l.wal.InsertLedger(data)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshes = append(l.refreshes, r)
}

func (l *Ledger) ReplayFromWAL(entryData []byte) error {
	var rec ledgerRecord
	if err := json.Unmarshal(entryData, &rec); err != nil {
		return err
	}
	switch rec.Table {
	case "local_commits":
		var c pb.LocalCommit
		if err := json.Unmarshal(rec.Data, &c); err != nil {
			return err
		}
		l.insertCommitLocked(&c)
	case "push_outcomes":
		var o pb.PushOutcome
		if err := json.Unmarshal(rec.Data, &o); err != nil {
			return err
		}
		l.insertOutcomeLocked(&o)
	case "refresh_events":
		var r pb.RefreshEvent
		if err := json.Unmarshal(rec.Data, &r); err != nil {
			return err
		}
		l.refreshes = append(l.refreshes, &r)
	}
	return nil
}

func (l *Ledger) insertCommitLocked(c *pb.LocalCommit) {
	l.commits = append(l.commits, c)
}

func (l *Ledger) insertOutcomeLocked(o *pb.PushOutcome) {
	l.outcomes = append(l.outcomes, o)
}

func (l *Ledger) Query(req *pb.QueryLedgerRequest) *pb.QueryLedgerResponse {
	l.mu.RLock()
	defer l.mu.RUnlock()

	resp := &pb.QueryLedgerResponse{}
	kind := req.GetResultKind()

	if kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_ALL || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_COMMITS_ONLY || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_UNSPECIFIED {
		for _, c := range l.commits {
			if matchesCommitFilters(req, c) {
				resp.Commits = append(resp.Commits, c)
			}
		}
	}

	if kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_ALL || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_PUSH_OUTCOMES_ONLY || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_UNSPECIFIED {
		for _, o := range l.outcomes {
			if matchesOutcomeFilters(req, o) {
				resp.PushOutcomes = append(resp.PushOutcomes, o)
			}
		}
	}

	if kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_ALL || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_REFRESH_EVENTS_ONLY || kind == pb.LedgerResultKind_LEDGER_RESULT_KIND_UNSPECIFIED {
		for _, r := range l.refreshes {
			if matchesRefreshFilters(req, r) {
				resp.RefreshEvents = append(resp.RefreshEvents, r)
			}
		}
	}

	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = 50
	}

	total := len(resp.Commits) + len(resp.PushOutcomes) + len(resp.RefreshEvents)
	resp.TotalMatches = int32(total)

	if total > pageSize {
		resp.Commits = limitSlice(resp.Commits, pageSize)
		resp.PushOutcomes = limitSlice(resp.PushOutcomes, pageSize)
		resp.RefreshEvents = limitSlice(resp.RefreshEvents, pageSize)
	}

	return resp
}

func matchesCommitFilters(req *pb.QueryLedgerRequest, c *pb.LocalCommit) bool {
	if w := req.GetWorkspaceId(); w != "" && c.GetWorkspaceId() != w {
		return false
	}
	if p := req.GetPrincipalId(); p != "" && c.GetPrincipalId() != p {
		return false
	}
	if r := req.GetRepoStorageId(); r != "" && c.GetRepoStorageId() != r {
		return false
	}
	if l := req.GetLocalCommitId(); l != "" && c.GetLocalCommitId() != l {
		return false
	}
	if a := req.GetCreatedAfter(); a > 0 && c.GetTimestampUnix() < a {
		return false
	}
	if b := req.GetCreatedBefore(); b > 0 && c.GetTimestampUnix() > b {
		return false
	}
	return true
}

func matchesOutcomeFilters(req *pb.QueryLedgerRequest, o *pb.PushOutcome) bool {
	if w := req.GetWorkspaceId(); w != "" && o.GetWorkspaceId() != w {
		return false
	}
	if j := req.GetJobId(); j != "" && o.GetJobId() != j {
		return false
	}
	if r := req.GetRepoStorageId(); r != "" && o.GetRepoStorageId() != r {
		return false
	}
	if s := req.GetPushStatus(); s != "" && o.GetStatus() != s {
		return false
	}
	if b := req.GetBranch(); b != "" && o.GetBranch() != b {
		return false
	}
	if a := req.GetCreatedAfter(); a > 0 && o.GetTimestampUnix() < a {
		return false
	}
	if b := req.GetCreatedBefore(); b > 0 && o.GetTimestampUnix() > b {
		return false
	}
	return true
}

func matchesRefreshFilters(req *pb.QueryLedgerRequest, r *pb.RefreshEvent) bool {
	if w := req.GetWorkspaceId(); w != "" && r.GetWorkspaceId() != w {
		return false
	}
	if s := req.GetRepoStorageId(); s != "" && r.GetRepoStorageId() != s {
		return false
	}
	if a := req.GetCreatedAfter(); a > 0 && r.GetTimestampUnix() < a {
		return false
	}
	if b := req.GetCreatedBefore(); b > 0 && r.GetTimestampUnix() > b {
		return false
	}
	return true
}

func limitSlice[T any](s []T, limit int) []T {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
