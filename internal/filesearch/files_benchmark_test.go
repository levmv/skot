package filesearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkCompileDirectPatternSlashHeavy(b *testing.B) {
	for _, depth := range []int{250, 500, 1000} {
		b.Run(fmt.Sprintf("segments-%d", depth), func(b *testing.B) {
			pattern := strings.Repeat("segment/", depth) + "leaf.go"
			b.ReportAllocs()
			b.SetBytes(int64(len(pattern)))
			for b.Loop() {
				if _, err := compileDirectPattern(pattern); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFilesManySiblingIgnoreFiles(b *testing.B) {
	const directories = 256
	root := b.TempDir()
	for index := range directories {
		directory := filepath.Join(root, fmt.Sprintf("dir-%03d", index))
		if err := os.MkdirAll(directory, 0o750); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte("*.tmp\n"), 0o600); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("fixture\n"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	searcher, err := New(root, Options{})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		results := 0
		if _, filesErr := searcher.Files(context.Background(), FilesQuery{Glob: "!*.none"}, func(File) error {
			results++
			return nil
		}); filesErr != nil {
			b.Fatal(filesErr)
		}
		if results != directories*2 {
			b.Fatalf("results = %d, want %d", results, directories*2)
		}
	}
}

func BenchmarkFilesDeepIgnoreFiles(b *testing.B) {
	benchmarkFilesDeepIgnoreFiles(b, "*.never\n")
}

func BenchmarkFilesDeepGlobstarContents(b *testing.B) {
	benchmarkFilesDeepIgnoreFiles(b, "**/never/**\n")
}

func benchmarkFilesDeepIgnoreFiles(b *testing.B, pattern string) {
	for _, depth := range []int{50, 100, 200, 400} {
		b.Run(fmt.Sprintf("depth-%d", depth), func(b *testing.B) {
			root := b.TempDir()
			directory := root
			for index := range depth {
				if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte(pattern), 0o600); err != nil {
					b.Fatal(err)
				}
				directory = filepath.Join(directory, fmt.Sprintf("d%03d", index))
				if err := os.Mkdir(directory, 0o750); err != nil {
					b.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, "leaf.txt"), []byte("fixture\n"), 0o600); err != nil {
				b.Fatal(err)
			}
			searcher, err := New(root, Options{})
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			for b.Loop() {
				results := 0
				if _, filesErr := searcher.Files(context.Background(), FilesQuery{Glob: "!*.none"}, func(File) error {
					results++
					return nil
				}); filesErr != nil {
					b.Fatal(filesErr)
				}
				if results != depth+1 {
					b.Fatalf("results = %d, want %d", results, depth+1)
				}
			}
		})
	}
}
