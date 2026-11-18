package layout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ErrManifestNotFound is returned by a ManifestStore when a shard has no manifest.
var ErrManifestNotFound = errors.New("manifest not found")

// ErrVersionConflict is returned by CompareAndSwap when the manifest changed
// since the caller read it. Callers must re-read and retry.
var ErrVersionConflict = errors.New("manifest version conflict")

// PackRef describes one pack in a shard manifest.
type PackRef struct {
	ID          string `json:"id"`
	Bytes       int64  `json:"bytes"`
	Blobs       int64  `json:"blobs"`
	CreatedUnix int64  `json:"created_unix"`
	Sealed      bool   `json:"sealed"`
}

// RetiredPack is a pack that has been superseded but is still readable during
// the compaction grace period.
type RetiredPack struct {
	PackRef
	RetiredUnix int64 `json:"retired_unix"`
}

// Manifest is the versioned, per-shard index of packs. It is the only metadata
// a read needs besides the pack index sidecars.
type Manifest struct {
	Layout      string        `json:"layout"`
	Shard       string        `json:"shard"`
	Version     uint64        `json:"version"`
	Packs       []PackRef     `json:"packs"`
	Retired     []RetiredPack `json:"retired,omitempty"`
	UpdatedUnix int64         `json:"updated_unix"`
}

// NewManifest returns an empty manifest for a shard.
func NewManifest(shard string) *Manifest {
	return &Manifest{
		Layout:      Version,
		Shard:       shard,
		Version:     0,
		UpdatedUnix: time.Now().UTC().Unix(),
	}
}

// Clone returns a deep copy so callers can mutate before a CAS without racing
// with readers of the original.
func (m *Manifest) Clone() *Manifest {
	if m == nil {
		return nil
	}
	out := *m
	out.Packs = append([]PackRef(nil), m.Packs...)
	out.Retired = append([]RetiredPack(nil), m.Retired...)
	return &out
}

// ActivePacks returns a copy of the currently readable packs.
func (m *Manifest) ActivePacks() []PackRef {
	if m == nil {
		return nil
	}
	return append([]PackRef(nil), m.Packs...)
}

// ActivePackIDs returns the IDs of the currently readable packs.
func (m *Manifest) ActivePackIDs() []string {
	ids := make([]string, 0, len(m.Packs))
	for _, p := range m.Packs {
		ids = append(ids, p.ID)
	}
	return ids
}

// Find returns the active pack with id.
func (m *Manifest) Find(id string) (PackRef, bool) {
	for _, p := range m.Packs {
		if p.ID == id {
			return p, true
		}
	}
	return PackRef{}, false
}

// AddPack appends an active pack. Duplicate IDs are rejected.
func (m *Manifest) AddPack(p PackRef) error {
	if p.ID == "" {
		return fmt.Errorf("layout: pack id is required")
	}
	if _, ok := m.Find(p.ID); ok {
		return fmt.Errorf("layout: pack %q already present", p.ID)
	}
	m.Packs = append(m.Packs, p)
	m.sortPacks()
	return nil
}

// Seal marks a pack as sealed (no longer appended to).
func (m *Manifest) Seal(id string) {
	for i := range m.Packs {
		if m.Packs[i].ID == id {
			m.Packs[i].Sealed = true
			return
		}
	}
}

// UnsealedPack returns the single writable pack, if any.
func (m *Manifest) UnsealedPack() (PackRef, bool) {
	for _, p := range m.Packs {
		if !p.Sealed {
			return p, true
		}
	}
	return PackRef{}, false
}

// Retire moves active packs to the retired list. It returns the IDs that were
// actually retired. Retiring is idempotent: unknown IDs are ignored.
func (m *Manifest) Retire(ids []string, now time.Time) []string {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	kept := m.Packs[:0]
	var retired []string
	for _, p := range m.Packs {
		if _, ok := want[p.ID]; !ok {
			kept = append(kept, p)
			continue
		}
		m.Retired = append(m.Retired, RetiredPack{
			PackRef:     p,
			RetiredUnix: now.UTC().Unix(),
		})
		retired = append(retired, p.ID)
	}
	m.Packs = kept
	m.sortRetired()
	return retired
}

// RemoveRetired drops a retired entry after its pack has been deleted.
func (m *Manifest) RemoveRetired(id string) {
	kept := m.Retired[:0]
	for _, p := range m.Retired {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	m.Retired = kept
}

// ExpiredRetired returns retired pack IDs whose grace period has elapsed.
func (m *Manifest) ExpiredRetired(now time.Time, grace time.Duration) []string {
	if grace < 0 {
		grace = 0
	}
	cutoff := now.UTC().Unix() - int64(grace/time.Second)
	var ids []string
	for _, p := range m.Retired {
		if p.RetiredUnix <= cutoff {
			ids = append(ids, p.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Validate checks the structural invariants of a manifest.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("layout: nil manifest")
	}
	if m.Layout != Version {
		return fmt.Errorf("layout: unexpected layout %q", m.Layout)
	}
	if err := ValidateShard(m.Shard); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(m.Packs)+len(m.Retired))
	for _, p := range m.Packs {
		if p.ID == "" {
			return fmt.Errorf("layout: empty pack id")
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("layout: duplicate pack id %q", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
	for _, p := range m.Retired {
		if p.ID == "" {
			return fmt.Errorf("layout: empty retired pack id")
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("layout: retired pack %q also active", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
	return nil
}

func (m *Manifest) sortPacks() {
	sort.Slice(m.Packs, func(i, j int) bool { return m.Packs[i].ID < m.Packs[j].ID })
}

func (m *Manifest) sortRetired() {
	sort.Slice(m.Retired, func(i, j int) bool { return m.Retired[i].ID < m.Retired[j].ID })
}

// ManifestStore reads and compare-and-swaps per-shard manifests. The store is
// responsible for assigning a new version and returning an opaque etag.
type ManifestStore interface {
	Read(ctx context.Context, shard string) (*Manifest, string, error)
	CompareAndSwap(ctx context.Context, shard, expectedETag string, next *Manifest) (string, error)
}

// MemoryManifestStore is an in-memory ManifestStore for tests and single-node
// use. It is safe for concurrent use.
type MemoryManifestStore struct {
	mu   sync.Mutex
	data map[string]*Manifest
}

// NewMemoryManifestStore returns an empty in-memory manifest store.
func NewMemoryManifestStore() *MemoryManifestStore {
	return &MemoryManifestStore{data: make(map[string]*Manifest)}
}

// Read implements ManifestStore.
func (s *MemoryManifestStore) Read(_ context.Context, shard string) (*Manifest, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data[shard]
	if !ok {
		return nil, "", ErrManifestNotFound
	}
	return m.Clone(), strconv.FormatUint(m.Version, 10), nil
}

// CompareAndSwap implements ManifestStore.
func (s *MemoryManifestStore) CompareAndSwap(_ context.Context, shard, expectedETag string, next *Manifest) (string, error) {
	if next == nil {
		return "", fmt.Errorf("layout: nil manifest")
	}
	if next.Shard != shard {
		return "", fmt.Errorf("layout: manifest shard %q does not match %q", next.Shard, shard)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.data[shard]
	if !ok {
		if expectedETag != "" {
			return "", ErrVersionConflict
		}
		cp := next.Clone()
		cp.Version = 1
		cp.UpdatedUnix = time.Now().UTC().Unix()
		s.data[shard] = cp
		return strconv.FormatUint(cp.Version, 10), nil
	}

	if expectedETag != strconv.FormatUint(current.Version, 10) {
		return "", ErrVersionConflict
	}
	cp := next.Clone()
	cp.Version = current.Version + 1
	cp.UpdatedUnix = time.Now().UTC().Unix()
	s.data[shard] = cp
	return strconv.FormatUint(cp.Version, 10), nil
}
