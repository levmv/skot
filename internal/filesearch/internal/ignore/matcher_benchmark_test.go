package ignore

import (
	"fmt"
	"strings"
	"testing"
)

func benchmarkRules(b *testing.B, extra int) *RuleSet {
	b.Helper()
	var patterns strings.Builder
	patterns.WriteString(`*.log
*.tmp
node_modules/
vendor/
build/
**/.env
**/*.min.js
!important.log
/config/local.yml
*.[oa]
`)
	for index := range extra {
		fmt.Fprintf(&patterns, "generated_%d_*.txt\n", index)
	}
	rules, diagnostics := Compile([]byte(patterns.String()), "", "benchmark")
	if len(diagnostics) != 0 {
		b.Fatal(diagnostics)
	}
	return rules
}

func BenchmarkDecideHit(b *testing.B) {
	rules := benchmarkRules(b, 0)
	b.ReportAllocs()
	for b.Loop() {
		_ = rules.Decide("src/app.log", false)
	}
}

func BenchmarkDecideMissLargeRuleSet(b *testing.B) {
	rules := benchmarkRules(b, 200)
	b.ReportAllocs()
	for b.Loop() {
		_ = rules.Decide("src/components/Button.tsx", false)
	}
}

func BenchmarkDecideDirectHit(b *testing.B) {
	rules := benchmarkRules(b, 0)
	b.ReportAllocs()
	for b.Loop() {
		_ = rules.DecideDirect("src/app.log", false)
	}
}

func BenchmarkDecideDirectMissLargeRuleSet(b *testing.B) {
	rules := benchmarkRules(b, 200)
	b.ReportAllocs()
	for b.Loop() {
		_ = rules.DecideDirect("src/components/Button.tsx", false)
	}
}
