package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tests := []struct {
		name, value string
		want        time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "2", 2 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"not a number or date", "soon", 0},
		{"a date", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"a past date", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
		// Multiplying these by a second would overflow, and could wrap around to something
		// small or negative. A huge hint has to stay huge, so that callers can cap it.
		{"past the point of overflow", "10000000000", maxRetryAfter},
		{"wraps around to under a second without a clamp", "18446744074", maxRetryAfter},
		{"too large to parse", "99999999999999999999", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseRetryAfter(tt.value, now); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
