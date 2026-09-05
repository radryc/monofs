package workspacestore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const maxSegmentSize = 64 * 1024 * 1024

type walWriter struct {
	dir    string
	mu     sync.Mutex
	logger *slog.Logger

	activeFile  *os.File
	segmentIdx  uint64
	segmentSize int64
	fsync       bool
}

func newWALWriter(dir string, logger *slog.Logger, fsync bool) (*walWriter, error) {
	walDir := filepath.Join(dir, "wal")
	if err := os.MkdirAll(walDir, 0755); err != nil {
		return nil, fmt.Errorf("create WAL dir: %w", err)
	}

	w := &walWriter{
		dir:    walDir,
		logger: logger,
		fsync:  fsync,
	}

	existing := w.listSegments()
	if len(existing) > 0 {
		w.segmentIdx = existing[len(existing)-1]
	} else {
		w.segmentIdx = 1
	}

	if err := w.openSegment(w.segmentIdx, true); err != nil {
		return nil, fmt.Errorf("open WAL segment: %w", err)
	}

	return w, nil
}

func (w *walWriter) segmentPath(idx uint64) string {
	return filepath.Join(w.dir, fmt.Sprintf("wal-%06d.log", idx))
}

func (w *walWriter) openSegment(idx uint64, create bool) error {
	if w.activeFile != nil {
		w.activeFile.Close()
	}

	path := w.segmentPath(idx)
	flag := os.O_APPEND | os.O_WRONLY
	if create {
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flag, 0644)
	if err != nil {
		return err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}

	w.activeFile = f
	w.segmentIdx = idx
	w.segmentSize = fi.Size()
	return nil
}

func (w *walWriter) Append(entry WALEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal WAL entry: %w", err)
	}
	data = append(data, '\n')

	if w.segmentSize > 0 && w.segmentSize+int64(len(data)) > maxSegmentSize {
		if err := w.rotateSegmentLocked(); err != nil {
			return fmt.Errorf("rotate WAL segment: %w", err)
		}
	}

	n, err := w.activeFile.Write(data)
	if err != nil {
		return fmt.Errorf("write WAL entry: %w", err)
	}
	w.segmentSize += int64(n)

	if w.fsync {
		w.activeFile.Sync()
	}

	return nil
}

func (w *walWriter) rotateSegmentLocked() error {
	if w.activeFile != nil {
		w.activeFile.Close()
	}
	w.segmentIdx++
	return w.openSegment(w.segmentIdx, true)
}

func (w *walWriter) TotalSize() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	var total int64
	for _, seg := range w.listSegments() {
		path := w.segmentPath(seg)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total
}

func (w *walWriter) listSegments() []uint64 {
	var indices []uint64
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "wal-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		numStr := strings.TrimPrefix(name, "wal-")
		numStr = strings.TrimSuffix(numStr, ".log")
		idx, err := strconv.ParseUint(numStr, 10, 64)
		if err != nil {
			continue
		}
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	return indices
}

func (w *walWriter) ReplayEntries(aboveSeq uint64) ([]WALEntry, error) {
	var all []WALEntry
	segments := w.listSegments()
	for _, seg := range segments {
		entries, err := readSegmentEntries(w.segmentPath(seg))
		if err != nil && !isScannerRecoverable(err) {
			w.logger.Error("error reading WAL segment during replay", "segment", seg, "error", err)
			return nil, fmt.Errorf("replay segment wal-%06d.log: %w", seg, err)
		}
		for _, entry := range entries {
			if entry.Seq > aboveSeq {
				all = append(all, entry)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	return all, nil
}

func isScannerRecoverable(err error) bool {
	return errors.Is(err, bufio.ErrFinalToken) || errors.Is(err, bufio.ErrTooLong)
}

func (w *walWriter) DeleteSegmentsBelow(belowSeq uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, seg := range w.listSegments() {
		if seg == w.segmentIdx {
			continue
		}
		path := w.segmentPath(seg)
		entries, err := readSegmentEntries(path)
		if err != nil {
			w.logger.Warn("error reading segment for deletion check", "segment", seg, "error", err)
			continue
		}

		allBelow := true
		for _, entry := range entries {
			if entry.Seq > belowSeq {
				allBelow = false
				break
			}
		}

		if allBelow && len(entries) > 0 {
			if err := os.Remove(path); err != nil {
				w.logger.Error("failed to delete WAL segment", "path", path, "error", err)
			} else {
				w.logger.Debug("deleted compacted WAL segment", "path", path)
			}
		}
	}
	return nil
}

func (w *walWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeFile != nil {
		w.activeFile.Close()
		w.activeFile = nil
	}
	return nil
}

func readSegmentEntries(path string) ([]WALEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []WALEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry WALEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return entries, scanErr
	}
	return entries, nil
}
