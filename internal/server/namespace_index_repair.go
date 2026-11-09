package server

import (
	"fmt"

	"github.com/radryc/monofs/internal/metastore"
)

// RepairRepositoryNamespaceIndex forces a rebuild of the repository namespace
// index for an offline server database directory. It is intended for recovery
// after manual database surgery or a legacy import.
func RepairRepositoryNamespaceIndex(dbPath string) error {
	db, err := metastore.Open(dbPath, metastore.Options{})
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer db.Close()

	s := &Server{db: db}
	if err := db.Update(func(tx metastore.Tx) error {
		if err := tx.Delete(bucketSchema, []byte(repoNamespaceIndexKey)); err != nil && err != metastore.ErrKeyNotFound {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("clear namespace index marker: %w", err)
	}
	if err := s.ensureRepoNamespaceIndex(); err != nil {
		return fmt.Errorf("rebuild repository namespace index: %w", err)
	}
	return nil
}
