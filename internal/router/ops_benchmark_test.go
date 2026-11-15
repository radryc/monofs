package router

import (
	"testing"
)

var benchRepoURLs = []struct {
	name string
	url  string
}{
	{"https", "https://github.com/owner/repo"},
	{"https_git", "https://github.com/owner/repo.git"},
	{"bare", "github.com/owner/repo"},
	{"go_module", "github.com/google/uuid@v1.3.0"},
	{"long_path", "https://github.com/org/very-long-repository-name/with/deep/path"},
}

func BenchmarkNormalizeRepoID(b *testing.B) {
	for _, tc := range benchRepoURLs {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = normalizeRepoID(tc.url)
			}
		})
	}
}
