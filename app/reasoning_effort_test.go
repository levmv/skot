package app

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	for input, want := range map[string]string{"": "", " default ": "", " HIGH ": "high"} {
		if got, err := normalizeReasoningEffort("deepseek/model", input); err != nil || got != want {
			t.Fatalf("effort %q = %q, %v", input, got, err)
		}
	}
	if _, err := normalizeReasoningEffort("deepseek/model", "medium"); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}
