package fuse

import (
	"bytes"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Merge markers used when a 3-way merge cannot resolve a region
// automatically. Style follows git's diff3 conflict presentation.
const (
	mergeConflictStart  = "<<<<<<< MONOFS-LOCAL"
	mergeConflictBase   = "||||||| BASE"
	mergeConflictSep    = "======="
	mergeConflictEnd    = ">>>>>>> MONOFS-UPSTREAM"
	mergeMissingNewline = "\n\\ No newline at end of file\n"
)

// Merge3 performs a line-based three-way merge of local changes (ours)
// and refreshed upstream content (theirs) against the original base
// content. Clean regions merge automatically; regions changed by both
// sides are emitted with conflict markers and counted.
func Merge3(base, ours, theirs []byte) (merged []byte, conflicts int) {
	baseLines := splitLines(base)
	oursLines := splitLines(ours)
	theirsLines := splitLines(theirs)

	result, conflicts := merge3Lines(baseLines, oursLines, theirsLines)
	return []byte(strings.Join(result, "")), conflicts
}

// merge3Lines merges three line slices. Every line retains its trailing
// newline (the final line may lack one).
func merge3Lines(base, ours, theirs []string) ([]string, int) {
	syncRegions := findSyncRegions(base, ours, theirs)

	var result []string
	conflicts := 0

	basePos, oursPos, theirsPos := 0, 0, 0
	for _, region := range syncRegions {
		// Merge the unsynchronized region that precedes this sync
		// region.
		unsynced := mergeUnsyncedRegion(
			base[basePos:region.BaseStart],
			ours[oursPos:region.OursStart],
			theirs[theirsPos:region.TheirsStart],
		)
		result = append(result, unsynced.lines...)
		conflicts += unsynced.conflicts

		// Copy the synchronized (agreed) region verbatim.
		result = append(result, base[region.BaseStart:region.BaseEnd]...)

		basePos = region.BaseEnd
		oursPos = region.OursEnd
		theirsPos = region.TheirsEnd
	}

	// Merge the trailing unsynchronized region.
	unsynced := mergeUnsyncedRegion(
		base[basePos:],
		ours[oursPos:],
		theirs[theirsPos:],
	)
	result = append(result, unsynced.lines...)
	conflicts += unsynced.conflicts

	return result, conflicts
}

type syncRegion struct {
	BaseStart, BaseEnd     int
	OursStart, OursEnd     int
	TheirsStart, TheirsEnd int
}

// findSyncRegions returns the maximal regions in which base, ours, and
// theirs all agree, computed as the intersection of the base<->ours
// and base<->theirs matching blocks.
func findSyncRegions(base, ours, theirs []string) []syncRegion {
	oursMatches := difflib.NewMatcher(base, ours).GetMatchingBlocks()
	theirsMatches := difflib.NewMatcher(base, theirs).GetMatchingBlocks()

	var regions []syncRegion
	oi, ti := 0, 0
	for oi < len(oursMatches) && ti < len(theirsMatches) {
		om, tm := oursMatches[oi], theirsMatches[ti]

		// Intersect the base ranges of the two match blocks.
		start := max(om.A, tm.A)
		end := min(om.A+om.Size, tm.A+tm.Size)
		if end > start {
			regions = append(regions, syncRegion{
				BaseStart:   start,
				BaseEnd:     end,
				OursStart:   om.B + (start - om.A),
				OursEnd:     om.B + (end - om.A),
				TheirsStart: tm.B + (start - tm.A),
				TheirsEnd:   tm.B + (end - tm.A),
			})
		}

		// Advance whichever block ends first in the base sequence.
		if om.A+om.Size < tm.A+tm.Size {
			oi++
		} else if om.A+om.Size > tm.A+tm.Size {
			ti++
		} else {
			oi++
			ti++
		}
	}

	// Coalesce adjacent regions so mergeUnsyncedRegion never sees a
	// zero-length unsynchronized gap created by block boundaries.
	var merged []syncRegion
	for _, region := range regions {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.BaseEnd == region.BaseStart && last.OursEnd == region.OursStart && last.TheirsEnd == region.TheirsStart {
				last.BaseEnd = region.BaseEnd
				last.OursEnd = region.OursEnd
				last.TheirsEnd = region.TheirsEnd
				continue
			}
		}
		merged = append(merged, region)
	}
	return merged
}

type unsyncedResult struct {
	lines     []string
	conflicts int
}

// mergeUnsyncedRegion merges one unsynchronized region. When only one
// side changed the region, that side wins; when both changed it
// identically the change is taken once; otherwise the region is a
// conflict and is emitted with diff3-style markers.
func mergeUnsyncedRegion(base, ours, theirs []string) unsyncedResult {
	oursChanged := !equalLines(base, ours)
	theirsChanged := !equalLines(base, theirs)

	switch {
	case !oursChanged && !theirsChanged:
		return unsyncedResult{lines: base}
	case !oursChanged:
		return unsyncedResult{lines: theirs}
	case !theirsChanged:
		return unsyncedResult{lines: ours}
	case equalLines(ours, theirs):
		return unsyncedResult{lines: ours}
	default:
		return unsyncedResult{
			lines:     conflictRegion(base, ours, theirs),
			conflicts: 1,
		}
	}
}

func conflictRegion(base, ours, theirs []string) []string {
	lines := []string{mergeConflictStart + "\n"}
	lines = append(lines, ours...)
	lines = append(lines, mergeConflictBase+"\n")
	lines = append(lines, base...)
	lines = append(lines, mergeConflictSep+"\n")
	lines = append(lines, theirs...)
	lines = append(lines, mergeConflictEnd+"\n")
	return lines
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitLines splits content into lines, each retaining its trailing
// newline. A trailing chunk without a newline is kept as-is.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	// Fast path: split on '\n' and re-append, dropping the empty tail.
	parts := strings.Split(string(content), "\n")
	lines := make([]string, 0, len(parts))
	for i := 0; i < len(parts)-1; i++ {
		lines = append(lines, parts[i]+"\n")
	}
	if tail := parts[len(parts)-1]; tail != "" {
		lines = append(lines, tail)
	}
	return lines
}

// hasConflictMarkers reports whether content contains MonoFS merge
// conflict markers, used to detect unresolved conflicts before
// staging or committing.
func hasConflictMarkers(content []byte) bool {
	return bytes.Contains(content, []byte(mergeConflictStart)) ||
		bytes.Contains(content, []byte(mergeConflictEnd))
}
