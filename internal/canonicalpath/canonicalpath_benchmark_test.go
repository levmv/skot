package canonicalpath

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func BenchmarkResolveMissingSuffix(b *testing.B) {
	parent := b.TempDir()
	for _, length := range []int{1 << 10, 1 << 16, 1 << 20} {
		b.Run(strconv.Itoa(length), func(b *testing.B) {
			path := filepath.Join(parent, strings.Repeat("missing/", length/8))
			b.ReportAllocs()
			b.SetBytes(int64(len(path)))
			for b.Loop() {
				Resolve(path)
			}
		})
	}
}
