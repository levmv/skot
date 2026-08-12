package agent

import "testing"

func TestEstimateTextTokensBalancesUTF8Scripts(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{text: "abcd", want: 1},
		{text: "тест", want: 3},
		{text: "测试", want: 2},
		{text: "🙂", want: 2},
	}
	for _, test := range tests {
		if got := estimateTextTokens(test.text); got != test.want {
			t.Fatalf("estimateTextTokens(%q) = %d, want %d", test.text, got, test.want)
		}
	}
}
