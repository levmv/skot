package modelhttp

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty"},
		{name: "delay seconds", value: " 15 ", want: 15 * time.Second},
		{name: "zero delay", value: "0"},
		{name: "negative delay", value: "-2"},
		{name: "future date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "elapsed date", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "invalid", value: "later"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseRetryAfter(test.value, now); got != test.want {
				t.Fatalf("ParseRetryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}
