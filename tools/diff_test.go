package tools

import (
	"math/rand"
	"slices"
	"testing"
)

func TestBuildFileChangeMetaCreatesFocusedHunks(t *testing.T) {
	old := []byte("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\n")
	updated := []byte("one\nTWO\nthree\nfour\nfive\nsix\nseven\nEIGHT\nnine\n")
	meta := buildFileChangeMeta("note.txt", "edited", old, updated)

	if meta.Additions != 2 || meta.Deletions != 2 || meta.TotalHunks != 2 || meta.Truncated {
		t.Fatalf("meta = %#v", meta)
	}
	if meta.Hunks[0].Lines[0].OldLine != 1 || meta.Hunks[0].Lines[0].NewLine != 1 {
		t.Fatalf("first hunk line numbers = %#v", meta.Hunks[0])
	}
}

func TestBuildFileChangeMetaDetectsMissingFinalNewline(t *testing.T) {
	meta := buildFileChangeMeta("note.txt", "edited", []byte("same"), []byte("same\n"))
	if meta.Additions != 1 || meta.Deletions != 1 || len(meta.Hunks) != 1 {
		t.Fatalf("newline-only meta = %#v", meta)
	}
	var oldMissing, newMissing bool
	for _, line := range meta.Hunks[0].Lines {
		if line.Kind == "delete" {
			oldMissing = line.NoNewline
		}
		if line.Kind == "add" {
			newMissing = line.NoNewline
		}
	}
	if !oldMissing || newMissing {
		t.Fatalf("newline markers = %#v", meta.Hunks[0].Lines)
	}
}

func TestMyersLineDiffReconstructsBothInputs(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	values := []string{"a\n", "b\n", "c\n", "d", "e\n"}
	for iteration := range 200 {
		old := make([]diffSourceLine, random.Intn(18))
		updated := make([]diffSourceLine, random.Intn(18))
		for index := range old {
			old[index] = splitDiffSourceLines(values[random.Intn(len(values))])[0]
		}
		for index := range updated {
			updated[index] = splitDiffSourceLines(values[random.Intn(len(values))])[0]
		}
		ops, exact := myersLineDiff(old, updated)
		if !exact {
			t.Fatal("small diff unexpectedly used fallback")
		}
		var reconstructedOld, reconstructedNew []diffSourceLine
		for _, op := range ops {
			if op.kind != '+' {
				reconstructedOld = append(reconstructedOld, op.line)
			}
			if op.kind != '-' {
				reconstructedNew = append(reconstructedNew, op.line)
			}
		}
		if !slices.Equal(reconstructedOld, old) || !slices.Equal(reconstructedNew, updated) {
			t.Fatalf("iteration %d reconstructed old=%#v new=%#v; want old=%#v new=%#v; ops=%#v", iteration, reconstructedOld, reconstructedNew, old, updated, ops)
		}
	}
}
