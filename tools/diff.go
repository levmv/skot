package tools

import (
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
)

const (
	diffContextLines        = 2
	maxDiffDisplayLines     = 80
	maxDiffDisplayLineRunes = 320
	maxMyersDiffDistance    = 512
)

type fileChange = agent.FileChange
type fileDiffHunk = agent.FileDiffHunk
type fileDiffLine = agent.FileDiffLine

type diffSourceLine struct {
	text       string
	hasNewline bool
}

type rawDiffOp struct {
	kind byte
	line diffSourceLine
}

type numberedDiffOp struct {
	kind    byte
	line    diffSourceLine
	oldLine int
	newLine int
}

func buildFileChangeMeta(path, operation string, old, updated []byte) fileChange {
	oldLines := splitDiffSourceLines(string(old))
	newLines := splitDiffSourceLines(string(updated))
	raw, exact := myersLineDiff(oldLines, newLines)
	if !exact {
		raw = fallbackLineDiff(oldLines, newLines)
	}
	ops := numberDiffOps(raw)
	meta := fileChange{
		Type:      agent.FileChangeDetailKind,
		Path:      path,
		Operation: operation,
	}
	for _, op := range ops {
		switch op.kind {
		case '+':
			meta.Additions++
		case '-':
			meta.Deletions++
		}
	}
	hunks := buildDiffHunks(ops)
	meta.TotalHunks = len(hunks)
	meta.Hunks, meta.Truncated = limitDiffHunks(hunks, maxDiffDisplayLines)
	return meta
}

func splitDiffSourceLines(text string) []diffSourceLine {
	if text == "" {
		return nil
	}
	parts := strings.SplitAfter(text, "\n")
	lines := make([]diffSourceLine, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		hasNewline := strings.HasSuffix(part, "\n")
		if hasNewline {
			part = strings.TrimSuffix(part, "\n")
		}
		lines = append(lines, diffSourceLine{text: part, hasNewline: hasNewline})
	}
	return lines
}

func myersLineDiff(old, updated []diffSourceLine) ([]rawDiffOp, bool) {
	maxDistance := len(old) + len(updated)
	if maxDistance == 0 {
		return nil, true
	}
	limit := min(maxDistance, maxMyersDiffDistance)
	frontier := map[int]int{1: 0}
	trace := make([]map[int]int, 0, limit+1)
	for distance := 0; distance <= limit; distance++ {
		trace = append(trace, maps.Clone(frontier))
		for diagonal := -distance; diagonal <= distance; diagonal += 2 {
			var x int
			if diagonal == -distance || diagonal != distance && frontier[diagonal-1] < frontier[diagonal+1] {
				x = frontier[diagonal+1]
			} else {
				x = frontier[diagonal-1] + 1
			}
			y := x - diagonal
			for x < len(old) && y < len(updated) && old[x] == updated[y] {
				x++
				y++
			}
			frontier[diagonal] = x
			if x >= len(old) && y >= len(updated) {
				return backtrackMyersDiff(trace, old, updated), true
			}
		}
	}
	return nil, false
}

func backtrackMyersDiff(trace []map[int]int, old, updated []diffSourceLine) []rawDiffOp {
	x, y := len(old), len(updated)
	reversed := make([]rawDiffOp, 0, x+y)
	for distance, frontier := range slices.Backward(trace) {
		diagonal := x - y
		previousDiagonal := diagonal - 1
		if diagonal == -distance || diagonal != distance && frontier[diagonal-1] < frontier[diagonal+1] {
			previousDiagonal = diagonal + 1
		}
		previousX := frontier[previousDiagonal]
		previousY := previousX - previousDiagonal
		for x > previousX && y > previousY {
			x--
			y--
			reversed = append(reversed, rawDiffOp{kind: ' ', line: old[x]})
		}
		if distance == 0 {
			break
		}
		if x == previousX {
			y--
			reversed = append(reversed, rawDiffOp{kind: '+', line: updated[y]})
		} else {
			x--
			reversed = append(reversed, rawDiffOp{kind: '-', line: old[x]})
		}
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func fallbackLineDiff(old, updated []diffSourceLine) []rawDiffOp {
	prefix := 0
	for prefix < len(old) && prefix < len(updated) && old[prefix] == updated[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(old)-prefix && suffix < len(updated)-prefix && old[len(old)-1-suffix] == updated[len(updated)-1-suffix] {
		suffix++
	}
	ops := make([]rawDiffOp, 0, len(old)+len(updated))
	for _, line := range old[:prefix] {
		ops = append(ops, rawDiffOp{kind: ' ', line: line})
	}
	for _, line := range old[prefix : len(old)-suffix] {
		ops = append(ops, rawDiffOp{kind: '-', line: line})
	}
	for _, line := range updated[prefix : len(updated)-suffix] {
		ops = append(ops, rawDiffOp{kind: '+', line: line})
	}
	for _, line := range old[len(old)-suffix:] {
		ops = append(ops, rawDiffOp{kind: ' ', line: line})
	}
	return ops
}

func numberDiffOps(raw []rawDiffOp) []numberedDiffOp {
	oldLine, newLine := 1, 1
	ops := make([]numberedDiffOp, 0, len(raw))
	for _, rawOp := range raw {
		op := numberedDiffOp{kind: rawOp.kind, line: rawOp.line}
		switch rawOp.kind {
		case ' ':
			op.oldLine, op.newLine = oldLine, newLine
			oldLine++
			newLine++
		case '-':
			op.oldLine = oldLine
			oldLine++
		case '+':
			op.newLine = newLine
			newLine++
		}
		ops = append(ops, op)
	}
	return ops
}

func buildDiffHunks(ops []numberedDiffOp) []fileDiffHunk {
	var changes []int
	for index, op := range ops {
		if op.kind != ' ' {
			changes = append(changes, index)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	var hunks []fileDiffHunk
	for changeIndex := 0; changeIndex < len(changes); {
		first := changes[changeIndex]
		last := first
		changeIndex++
		for changeIndex < len(changes) {
			next := changes[changeIndex]
			if next-last-1 > diffContextLines*2 {
				break
			}
			last = next
			changeIndex++
		}
		start := max(0, first-diffContextLines)
		end := min(len(ops), last+diffContextLines+1)
		hunks = append(hunks, makeDiffHunk(ops, start, end))
	}
	return hunks
}

func makeDiffHunk(ops []numberedDiffOp, start, end int) fileDiffHunk {
	oldCursor, newCursor := 1, 1
	for _, op := range ops[:start] {
		if op.kind != '+' {
			oldCursor++
		}
		if op.kind != '-' {
			newCursor++
		}
	}
	hunk := fileDiffHunk{OldStart: oldCursor, NewStart: newCursor}
	for _, op := range ops[start:end] {
		if op.kind != '+' {
			hunk.OldLines++
		}
		if op.kind != '-' {
			hunk.NewLines++
		}
		text, truncated := truncateDiffLine(op.line.text, maxDiffDisplayLineRunes)
		kind := "context"
		if op.kind == '+' {
			kind = "add"
		} else if op.kind == '-' {
			kind = "delete"
		}
		hunk.Lines = append(hunk.Lines, fileDiffLine{
			Kind:      kind,
			OldLine:   op.oldLine,
			NewLine:   op.newLine,
			Text:      text,
			NoNewline: !op.line.hasNewline,
			Truncated: truncated,
		})
	}
	if hunk.OldLines == 0 {
		hunk.OldStart = max(0, hunk.OldStart-1)
	}
	if hunk.NewLines == 0 {
		hunk.NewStart = max(0, hunk.NewStart-1)
	}
	return hunk
}

func truncateDiffLine(text string, limit int) (string, bool) {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\t", "    ")
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:max(1, limit-1)]) + "…", true
}

func limitDiffHunks(hunks []fileDiffHunk, limit int) ([]fileDiffHunk, bool) {
	if limit <= 0 {
		return nil, len(hunks) > 0
	}
	remaining := limit
	limited := make([]fileDiffHunk, 0, len(hunks))
	truncated := false
	for _, hunk := range hunks {
		if remaining == 0 {
			truncated = true
			break
		}
		copy := hunk
		if len(copy.Lines) > remaining {
			copy.Lines = append([]fileDiffLine(nil), copy.Lines[:remaining]...)
			truncated = true
		}
		for _, line := range copy.Lines {
			truncated = truncated || line.Truncated
		}
		limited = append(limited, copy)
		remaining -= len(copy.Lines)
	}
	return limited, truncated
}
