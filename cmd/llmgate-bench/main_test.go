package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/mock"
)

func TestARunMeasuresAndCountsWhatHappened(t *testing.T) {
	upstream := httptest.NewServer(mock.New())
	defer upstream.Close()
	t.Setenv(keyEnv, "mock")
	var out bytes.Buffer
	err := run(context.Background(), []string{
		"-url", upstream.URL + "/v1/chat/completions", "-model", "bench;tokens=4",
		"-stream", "-concurrency", "4", "-duration", "500ms", "-warmup", "100ms", "-label", "direct",
	}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if r.Label != "direct" || r.Requests == 0 || r.OK != r.Requests || r.Concurrency != 4 || !r.Stream {
		t.Fatalf("result = %+v", r)
	}
	if r.Latency.P50 <= 0 || r.Latency.P99 < r.Latency.P50 || r.Latency.Max < r.Latency.P99 || r.TTFB.P50 <= 0 || r.TTFB.P50 > r.Latency.P50 {
		t.Fatalf("latency = %+v, ttfb = %+v", r.Latency, r.TTFB)
	}
	if r.ThroughputRPS <= 0 || r.GoVersion == "" {
		t.Fatalf("result = %+v", r)
	}
}

// Warm-up requests aren't counted, and failures are counted by status, not hidden.
func TestWarmUpIsExcludedAndFailuresAreCounted(t *testing.T) {
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-url", server.URL, "-concurrency", "1", "-duration", "300ms", "-warmup", "200ms"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Errors["429"] == 0 || r.OK == 0 || r.OK+r.Errors["429"] != r.Requests {
		t.Fatalf("result = %+v", r)
	}
	if int64(r.Requests) >= served.Load() {
		t.Fatalf("warm-up was counted: %d of %d", r.Requests, served.Load())
	}
}

func TestTheKeyIsNeverInTheResult(t *testing.T) {
	upstream := httptest.NewServer(mock.New())
	defer upstream.Close()
	var out bytes.Buffer
	t.Setenv("LLMGATE_BENCH_KEY", "a-secret-key")
	if err := run(context.Background(), []string{"-url", upstream.URL + "/v1/chat/completions", "-duration", "100ms", "-warmup", "0s"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "a-secret-key") {
		t.Fatal("the key appears in the result")
	}
}

func TestPercentiles(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(100-i) * time.Millisecond
	}
	s := summarize(samples)
	if s.P50 != 50 || s.P95 != 95 || s.P99 != 99 || s.Max != 100 {
		t.Fatalf("summary = %+v", s)
	}
	if empty := summarize(nil); empty.P50 != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestCompareShowsWhatEachRunAdds(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, r result) string {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		path := dir + "/" + name
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	base := write("base.json", result{Label: "direct", Requests: 100, OK: 100, ThroughputRPS: 50, Latency: summary{P50: 1, P95: 2, P99: 3}, TTFB: summary{P50: 0.5}})
	gw := write("gw.json", result{Label: "gateway", Requests: 90, OK: 89, Errors: map[string]int{"502": 1}, ThroughputRPS: 45, Latency: summary{P50: 1.5, P95: 2.75, P99: 4}, TTFB: summary{P50: 0.75}})
	var out bytes.Buffer
	if err := run(context.Background(), []string{"compare", base, gw}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	table := out.String()
	for _, want := range []string{"| direct |", "| gateway |", "+0.50", "+0.75", "+1.00", "+0.25", "502: 1"} {
		if !strings.Contains(table, want) {
			t.Fatalf("missing %q in:\n%s", want, table)
		}
	}
}
