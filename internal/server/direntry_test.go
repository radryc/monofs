package server

import (
	"testing"

	"github.com/radryc/monofs/internal/metastore"
)

// readDirEntries returns a directory's children in name order using the new
// per-entry directory index.
func readDirEntries(t *testing.T, s *Server, storageID, dirPath string) []dirIndexEntry {
	t.Helper()
	var out []dirIndexEntry
	if err := s.db.View(func(tx metastore.Tx) error {
		entries, err := listDirEntries(tx, storageID, dirPath)
		if err != nil {
			return err
		}
		out = entries
		return nil
	}); err != nil {
		t.Fatalf("listDirEntries(%q): %v", dirPath, err)
	}
	return out
}

// countDirEntries returns the number of children in a directory.
func countDirEntries(t *testing.T, s *Server, storageID, dirPath string) int {
	t.Helper()
	return len(readDirEntries(t, s, storageID, dirPath))
}
