package filesearch

import "testing"

func FuzzCompileDirectPattern(f *testing.F) {
	for _, pattern := range []string{
		"*.go",
		"**/*.{go,txt}",
		"!*.tmp",
		"src/**",
		"[a-z]?.md",
		"",
		"{",
	} {
		f.Add(pattern)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		compiled, err := compileDirectPattern(pattern)
		if err != nil {
			return
		}
		_ = compiled.matches("nested/example.go", false)
		_ = compiled.matches("nested", true)
	})
}
