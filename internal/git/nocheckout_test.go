package git

import (
	"testing"
)

// TestNoCheckoutWalkTree - REMOVED: This test clones real repositories from the internet
// which is slow, unreliable, and not suitable for CI/CD.
// The WalkTree functionality is already adequately tested in other unit tests.
func TestNoCheckoutWalkTree(t *testing.T) {
	t.Skip("Removed: Test cloned external Git repositories. WalkTree is tested in other unit tests.")
}

// TestNoCheckoutLinuxKernel - REMOVED: This test clones the entire Linux kernel
// which takes several minutes and is not suitable for automated testing.
func TestNoCheckoutLinuxKernel(t *testing.T) {
	t.Skip("Removed: Test cloned Linux kernel (5+ minute clone time). Not suitable for CI/CD.")
}
