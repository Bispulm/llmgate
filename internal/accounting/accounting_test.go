package accounting

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
)

func logged(t *testing.T, a Attempt) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	NewLogRecorder(slog.New(slog.NewJSONHandler(&buf, nil))).RecordAttempt(context.Background(), a)
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	return got
}

func TestLogRecorderReportsAbsentUsageAsAbsent(t *testing.T) {
	got := logged(t, Attempt{RequestID: "r", Outcome: OutcomeInterrupted, Committed: true})
	if got["usage_state"] != UsageAbsent {
		t.Fatalf("usage_state = %v", got["usage_state"])
	}
	if _, ok := got["prompt_tokens"]; ok {
		t.Fatalf("absent usage logged counts: %v", got)
	}
}

func TestLogRecorderReportsUsageCounts(t *testing.T) {
	got := logged(t, Attempt{
		RequestID: "r",
		Outcome:   OutcomeCompleted,
		Usage:     &chat.Usage{State: chat.UsageInconsistent, PromptTokens: 3, CompletionTokens: 4, TotalTokens: 9},
	})
	if got["usage_state"] != string(chat.UsageInconsistent) {
		t.Fatalf("usage_state = %v", got["usage_state"])
	}
	if got["prompt_tokens"] != 3.0 || got["completion_tokens"] != 4.0 || got["total_tokens"] != 9.0 {
		t.Fatalf("counts = %v", got)
	}
}

func TestLogRecorderDoesNotTrustMalformedCounts(t *testing.T) {
	got := logged(t, Attempt{
		RequestID: "r",
		Usage:     &chat.Usage{State: chat.UsageMalformed, PromptTokens: 3},
	})
	if got["usage_state"] != string(chat.UsageMalformed) {
		t.Fatalf("usage_state = %v", got["usage_state"])
	}
	if _, ok := got["prompt_tokens"]; ok {
		t.Fatalf("malformed usage logged counts: %v", got)
	}
}

func TestLogRecorderIncludesFailureDetails(t *testing.T) {
	got := logged(t, Attempt{
		RequestID:      "req",
		TeamID:         "team",
		KeyID:          "key",
		Alias:          "alias",
		Provider:       "prov",
		Model:          "mod",
		Number:         1,
		Stream:         true,
		Outcome:        OutcomeFailed,
		ErrorKind:      provider.KindServer,
		Delivery:       provider.DeliveryConfirmed,
		UpstreamStatus: 500,
		UpstreamCode:   "server_error",
		Duration:       time.Second,
	})
	want := map[string]any{
		"request_id": "req", "team_id": "team", "key_id": "key", "alias": "alias", "provider": "prov",
		"model": "mod", "attempt": 1.0, "stream": true, "outcome": string(OutcomeFailed),
		"error_kind": string(provider.KindServer), "delivery": string(provider.DeliveryConfirmed),
		"upstream_status": 500.0, "upstream_code": "server_error",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
