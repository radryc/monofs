package workspaceledger

import (
	"encoding/json"
	"sort"
	"sync"

	pb "github.com/radryc/monofs/api/proto"
)

type WALWriter interface {
	InsertLedger(data []byte) error
}

type Ledger struct {
	mu sync.RWMutex
	wal WALWriter

	commits  []*pb.LocalCommit
	outcomes []*pb.PushOutcome
	refreshes []*pb.RefreshEvent

	byCommitID map[string]*pb.LocalCommit
	byJobID   map[string]*pb.PushOutcome
	byWorkspace map[string][]int
	byPrincipal map[string][]int
	byRepo      map[string][]int
	byStatus   map[string][]int
}

func New() *Ledger {
	return &Ledger{
		byCommitID:  make(map[string]*pb.LocalCommit),
		byJobID:     make(map[string]*pb.PushOutcome),
		byWorkspace: make(map[string][]int),
		byPrincipal: make(map[string][]int),
		byRepo:      make(map[string][]int),
		byStatus:    make(map[string][]int),
	}
}

func NewWithWAL(wal WALWriter) *Ledger {
	return &Ledger{
		wal:         wal,
		byCommitID:  make(map[string]*pb.LocalCommit),
		byJobID:     make(map[string]*pb.PushOutcome),
		byWorkspace: make(map[string][]int),
		byPrincipal: make(map[string][]int),
		byRepo:      make(map[string][]int),
		byStatus:    make(map[string][]int),
	}
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
	l.byWorkspace[r.GetWorkspaceId()] = append(l.byWorkspace[r.GetWorkspaceId()], len(l.refreshes)-1)
	l.byRepo[r.GetRepoStorageId()] = append(l.byRepo[r.GetRepoStorageId()], len(l.refreshes)-1)
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
		l.byWorkspace[r.GetWorkspaceId()] = append(l.byWorkspace[r.GetWorkspaceId()], len(l.refreshes)-1)
		l.byRepo[r.GetRepoStorageId()] = append(l.byRepo[r.GetRepoStorageId()], len(l.refreshes)-1)
	}
	return nil
}

func (l *Ledger) insertCommitLocked(c *pb.LocalCommit) {
	idx := len(l.commits)
	l.commits = append(l.commits, c)
	l.byCommitID[c.GetLocalCommitId()] = c
	l.byWorkspace[c.GetWorkspaceId()] = append(l.byWorkspace[c.GetWorkspaceId()], idx)
	l.byPrincipal[c.GetPrincipalId()] = append(l.byPrincipal[c.GetPrincipalId()], idx)
	l.byRepo[c.GetRepoStorageId()] = append(l.byRepo[c.GetRepoStorageId()], idx)
}

func (l *Ledger) insertOutcomeLocked(o *pb.PushOutcome) {
	idx := len(l.outcomes)
	l.outcomes = append(l.outcomes, o)
	l.byJobID[o.GetJobId()] = o
	l.byWorkspace[o.GetWorkspaceId()] = append(l.byWorkspace[o.GetWorkspaceId()], idx)
	l.byRepo[o.GetRepoStorageId()] = append(l.byRepo[o.GetRepoStorageId()], idx)
	statusKey := "outcome:" + o.GetStatus()
	l.byStatus[statusKey] = append(l.byStatus[statusKey], idx)
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
	if w := req.GetWorkspaceId(); w != "" && c.GetWorkspaceId() != w { return false }
	if p := req.GetPrincipalId(); p != "" && c.GetPrincipalId() != p { return false }
	if r := req.GetRepoStorageId(); r != "" && c.GetRepoStorageId() != r { return false }
	if l := req.GetLocalCommitId(); l != "" && c.GetLocalCommitId() != l { return false }
	if a := req.GetCreatedAfter(); a > 0 && c.GetTimestampUnix() < a { return false }
	if b := req.GetCreatedBefore(); b > 0 && c.GetTimestampUnix() > b { return false }
	return true
}

func matchesOutcomeFilters(req *pb.QueryLedgerRequest, o *pb.PushOutcome) bool {
	if w := req.GetWorkspaceId(); w != "" && o.GetWorkspaceId() != w { return false }
	if j := req.GetJobId(); j != "" && o.GetJobId() != j { return false }
	if r := req.GetRepoStorageId(); r != "" && o.GetRepoStorageId() != r { return false }
	if s := req.GetPushStatus(); s != "" && o.GetStatus() != s { return false }
	if b := req.GetBranch(); b != "" && o.GetBranch() != b { return false }
	if a := req.GetCreatedAfter(); a > 0 && o.GetTimestampUnix() < a { return false }
	if b := req.GetCreatedBefore(); b > 0 && o.GetTimestampUnix() > b { return false }
	return true
}

func matchesRefreshFilters(req *pb.QueryLedgerRequest, r *pb.RefreshEvent) bool {
	if w := req.GetWorkspaceId(); w != "" && r.GetWorkspaceId() != w { return false }
	if s := req.GetRepoStorageId(); s != "" && r.GetRepoStorageId() != s { return false }
	if a := req.GetCreatedAfter(); a > 0 && r.GetTimestampUnix() < a { return false }
	if b := req.GetCreatedBefore(); b > 0 && r.GetTimestampUnix() > b { return false }
	return true
}

func limitSlice[T any](s []T, limit int) []T {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func sortByTimestamp[T interface{ GetTimestampUnix() int64 }](s []T) {
	sort.Slice(s, func(i, j int) bool {
		return s[i].GetTimestampUnix() > s[j].GetTimestampUnix()
	})
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
