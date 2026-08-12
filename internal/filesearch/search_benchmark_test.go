package filesearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkSearchSynthetic(b *testing.B) {
	root := b.TempDir()
	line := "ordinary synthetic source line with enough bytes for throughput\n"
	content := strings.Repeat(line, 1023) + "needle synthetic match\n"
	var totalBytes int64
	for index := range 128 {
		path := filepath.Join(root, fmt.Sprintf("dir-%02d", index%16), fmt.Sprintf("file-%03d.txt", index))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		totalBytes += int64(len(content))
	}
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		b.Fatal(err)
	}
	benchmarkSearch(b, searcher, SearchQuery{Pattern: "needle", PreviewBytes: 300}, totalBytes)
}

func benchmarkSearch(b *testing.B, searcher *Searcher, query SearchQuery, bytesPerOperation int64) {
	b.Helper()
	if bytesPerOperation > 0 {
		b.SetBytes(bytesPerOperation)
	}
	b.ReportAllocs()
	b.ResetTimer()
	totalResults := 0
	for range b.N {
		results := 0
		if _, err := searcher.Search(context.Background(), query, func(Match) error {
			results++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		totalResults += results
	}
	b.ReportMetric(float64(totalResults)/float64(b.N), "results/op")
}
