package router

import (
	"hash/fnv"
	"sort"
)

const guardianMutationLockStripes = 256

// lockGuardianMutations locks the stripes for the supplied display paths in a
// deterministic order. It returns an unlock function. Different display paths
// that hash to different stripes do not serialize with each other.
func (r *Router) lockGuardianMutations(displayPaths ...string) func() {
	if len(displayPaths) == 0 {
		return func() {}
	}
	seen := make(map[int]struct{}, len(displayPaths))
	indexes := make([]int, 0, len(displayPaths))
	for _, displayPath := range displayPaths {
		h := fnv.New32a()
		_, _ = h.Write([]byte(displayPath))
		index := int(h.Sum32() % guardianMutationLockStripes)
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		r.guardianMutationLocks[index].Lock()
	}
	return func() {
		for i := len(indexes) - 1; i >= 0; i-- {
			r.guardianMutationLocks[indexes[i]].Unlock()
		}
	}
}
