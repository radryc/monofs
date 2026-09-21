// Package search provides symbol extraction for code search.
//
// Zoekt indexes symbol metadata (used by `sym:` queries) when a working
// universal-ctags binary is available. Symbol indexing is best-effort: when no
// ctags binary is present, documents are still indexed and full-text search
// works, but `sym:` queries return nothing. This file centralizes ctags
// detection so the indexer can advertise whether symbol search is enabled.
package search

import (
	"os"
	"os/exec"
	"strings"
)

// ctagsCommand locates a ctags binary to use for symbol extraction, preferring
// universal-ctags (which Zoekt uses natively). Resolution order:
//
//  1. The CTAGS_COMMAND environment variable (explicit override).
//  2. The `universal-ctags` binary on PATH.
//  3. The `ctags` binary on PATH.
//
// Empty string when no binary is found.
func ctagsCommand() string {
	if cmd := os.Getenv("CTAGS_COMMAND"); cmd != "" {
		return cmd
	}
	if path, err := exec.LookPath("universal-ctags"); err == nil {
		return path
	}
	if path, err := exec.LookPath("ctags"); err == nil {
		return path
	}
	return ""
}

// isUniversalCTags reports whether the given binary is a universal-ctags build
// with the +interactive feature that Zoekt requires for symbol extraction.
func isUniversalCTags(bin string) bool {
	if bin == "" {
		return false
	}
	out, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "+interactive")
}

// symbolSupport summarizes the symbol-extraction capability available on this
// host. EnableUniversalCtags is true when a Zoekt-compatible universal-ctags
// binary is present (symbols are extracted natively during indexing).
type symbolSupport struct {
	// Binary is the ctags path detected, or "".
	Binary string
	// EnableUniversalCtags is true when the detected binary is universal-ctags.
	EnableUniversalCtags bool
}

// detectSymbolSupport inspects the environment for a ctags binary.
func detectSymbolSupport() symbolSupport {
	bin := ctagsCommand()
	return symbolSupport{
		Binary:               bin,
		EnableUniversalCtags: bin != "" && isUniversalCTags(bin),
	}
}
