package git

import (
	"testing"
)

// BenchmarkCloneStrategies - REMOVED: This benchmark clones real repositories from the internet
// which is slow, unreliable, and not suitable for CI/CD.
// Clone performance is not a critical metric for unit testing.
func BenchmarkCloneStrategies(b *testing.B) {
	b.Skip("Removed: Benchmark cloned external Git repositories. Not suitable for CI/CD.")
}
