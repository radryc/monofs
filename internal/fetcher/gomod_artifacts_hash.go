package fetcher

import (
	"golang.org/x/mod/sumdb/dirhash"
)

// computeDirHash computes the h1: dirhash of a module zip file using
// the canonical golang.org/x/mod/sumdb/dirhash implementation.
// This produces hashes identical to what the Go toolchain expects.
func computeDirHash(zipPath string) (string, error) {
	return dirhash.HashZip(zipPath, dirhash.Hash1)
}
