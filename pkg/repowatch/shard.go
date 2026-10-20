// Package repowatch contains shared control-plane helpers for the MonoFS
// repository watcher and Guardian's repo importer: consistent shard assignment
// and Kubernetes Lease based leader election.
package repowatch

import "hash/fnv"

// ShardFor maps a stable key (storage ID or repo name) to a shard.
func ShardFor(key string, shards int) int {
	if shards <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(shards))
}
