package fuse

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nutsdb/nutsdb"
)

const currentLogicalBranchKey = "current_logical_branch"

func (odb *OverlayDB) putJSONValue(bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s value: %w", bucket, err)
	}

	return odb.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucket, []byte(key), data, 0)
	})
}

func (odb *OverlayDB) getJSONValue(bucket, key string, target any) (bool, error) {
	var found bool

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucket, []byte(key))
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		if err := json.Unmarshal(value, target); err != nil {
			return fmt.Errorf("unmarshal %s value: %w", bucket, err)
		}
		found = true
		return nil
	})

	return found, err
}

func (odb *OverlayDB) deleteBucketKey(bucket, key string) error {
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		err := tx.Delete(bucket, []byte(key))
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
}

func (odb *OverlayDB) countBucket(bucket string) int {
	count := 0
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucket)
		if err != nil {
			return nil
		}
		count = len(keys)
		return nil
	})
	return count
}

func (odb *OverlayDB) PutStagedEntry(path string, entry StagedIndexEntry) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("staged entry path is required")
	}
	if entry.Path == "" {
		entry.Path = path
	}
	return odb.putJSONValue(bucketOverlayStaged, path, entry)
}

func (odb *OverlayDB) GetStagedEntry(path string) (StagedIndexEntry, bool, error) {
	var entry StagedIndexEntry
	found, err := odb.getJSONValue(bucketOverlayStaged, path, &entry)
	return entry, found, err
}

func (odb *OverlayDB) DeleteStagedEntry(path string) error {
	return odb.deleteBucketKey(bucketOverlayStaged, path)
}

func (odb *OverlayDB) ListStagedEntries() ([]StagedIndexEntry, error) {
	entries := make([]StagedIndexEntry, 0)

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		keys, values, err := tx.GetAll(bucketOverlayStaged)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for idx, key := range keys {
			var entry StagedIndexEntry
			if err := json.Unmarshal(values[idx], &entry); err != nil {
				return fmt.Errorf("unmarshal staged entry: %w", err)
			}
			if entry.Path == "" {
				entry.Path = string(key)
			}
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Path < entries[right].Path
	})
	return entries, nil
}

func (odb *OverlayDB) ClearStagedEntries() error {
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayStaged)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, key := range keys {
			if err := tx.Delete(bucketOverlayStaged, key); err != nil && !isNotFound(err) {
				return err
			}
		}
		return nil
	})
}

func (odb *OverlayDB) StagedEntryCount() int {
	return odb.countBucket(bucketOverlayStaged)
}

func (odb *OverlayDB) PutLocalVirtualCommit(commit LocalVirtualCommit) error {
	commit.ID = strings.TrimSpace(commit.ID)
	if commit.ID == "" {
		return fmt.Errorf("local virtual commit id is required")
	}
	return odb.putJSONValue(bucketOverlayCommits, commit.ID, commit)
}

func (odb *OverlayDB) GetLocalVirtualCommit(id string) (LocalVirtualCommit, bool, error) {
	var commit LocalVirtualCommit
	found, err := odb.getJSONValue(bucketOverlayCommits, id, &commit)
	return commit, found, err
}

func (odb *OverlayDB) DeleteLocalVirtualCommit(id string) error {
	return odb.deleteBucketKey(bucketOverlayCommits, id)
}

func (odb *OverlayDB) ListLocalVirtualCommits() ([]LocalVirtualCommit, error) {
	commits := make([]LocalVirtualCommit, 0)

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		keys, values, err := tx.GetAll(bucketOverlayCommits)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for idx, key := range keys {
			var commit LocalVirtualCommit
			if err := json.Unmarshal(values[idx], &commit); err != nil {
				return fmt.Errorf("unmarshal local virtual commit: %w", err)
			}
			if commit.ID == "" {
				commit.ID = string(key)
			}
			commits = append(commits, commit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(commits, func(left, right int) bool {
		if commits[left].CreatedAt.Equal(commits[right].CreatedAt) {
			return commits[left].ID < commits[right].ID
		}
		return commits[left].CreatedAt.Before(commits[right].CreatedAt)
	})
	return commits, nil
}

func (odb *OverlayDB) LocalVirtualCommitCount() int {
	return odb.countBucket(bucketOverlayCommits)
}

func (odb *OverlayDB) SetCurrentLogicalBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return odb.deleteBucketKey(bucketOverlayBranch, currentLogicalBranchKey)
	}

	return odb.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketOverlayBranch, []byte(currentLogicalBranchKey), []byte(branch), 0)
	})
}

func (odb *OverlayDB) GetCurrentLogicalBranch() (string, bool, error) {
	var branch string
	var found bool

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketOverlayBranch, []byte(currentLogicalBranchKey))
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		branch = string(value)
		found = true
		return nil
	})

	return branch, found, err
}

func branchMappingKey(principalID, logicalBranch, storageID string) string {
	return strings.Join([]string{principalID, logicalBranch, storageID}, "\x1f")
}

func (odb *OverlayDB) PutBranchMapping(mapping SessionBranchMapping) error {
	mapping.PrincipalID = strings.TrimSpace(mapping.PrincipalID)
	mapping.LogicalBranch = strings.TrimSpace(mapping.LogicalBranch)
	mapping.StorageID = strings.TrimSpace(mapping.StorageID)
	if mapping.PrincipalID == "" || mapping.LogicalBranch == "" || mapping.StorageID == "" {
		return fmt.Errorf("branch mapping requires principal id, logical branch, and storage id")
	}
	if mapping.CreatedAt.IsZero() {
		mapping.CreatedAt = time.Now().UTC()
	}

	return odb.putJSONValue(bucketOverlayBranch, branchMappingKey(mapping.PrincipalID, mapping.LogicalBranch, mapping.StorageID), mapping)
}

func (odb *OverlayDB) GetBranchMapping(principalID, logicalBranch, storageID string) (SessionBranchMapping, bool, error) {
	var mapping SessionBranchMapping
	found, err := odb.getJSONValue(bucketOverlayBranch, branchMappingKey(principalID, logicalBranch, storageID), &mapping)
	return mapping, found, err
}

func (odb *OverlayDB) DeleteBranchMapping(principalID, logicalBranch, storageID string) error {
	return odb.deleteBucketKey(bucketOverlayBranch, branchMappingKey(principalID, logicalBranch, storageID))
}

func (odb *OverlayDB) ListBranchMappings() ([]SessionBranchMapping, error) {
	mappings := make([]SessionBranchMapping, 0)

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		keys, values, err := tx.GetAll(bucketOverlayBranch)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for idx, key := range keys {
			if string(key) == currentLogicalBranchKey {
				continue
			}
			var mapping SessionBranchMapping
			if err := json.Unmarshal(values[idx], &mapping); err != nil {
				return fmt.Errorf("unmarshal branch mapping: %w", err)
			}
			mappings = append(mappings, mapping)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(mappings, func(left, right int) bool {
		if mappings[left].PrincipalID != mappings[right].PrincipalID {
			return mappings[left].PrincipalID < mappings[right].PrincipalID
		}
		if mappings[left].LogicalBranch != mappings[right].LogicalBranch {
			return mappings[left].LogicalBranch < mappings[right].LogicalBranch
		}
		return mappings[left].StorageID < mappings[right].StorageID
	})
	return mappings, nil
}

func (odb *OverlayDB) BranchMappingCount() int {
	count := 0
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayBranch)
		if err != nil {
			return nil
		}
		for _, key := range keys {
			if string(key) == currentLogicalBranchKey {
				continue
			}
			count++
		}
		return nil
	})
	return count
}
