// Command llmgate-bench sends a fixed workload to a chat completions endpoint and
// reports latency, time to first byte, throughput, and errors. The same workload can run
// against the mock directly and through the gateway, so the difference is what the
// gateway adds. Warm-up requests are sent but not counted.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// keyEnv names the variable holding the API key, so it never appears in a process list
// or in the result.
const keyEnv = "LLMGATE_BENCH_KEY"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate-bench:", err)
		os.Exit(1)
	}
}

// summary is a distribution in milliseconds.
type summary struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

type result struct {
	Label         string         `json:"label"`
	URL           string         `json:"url"`
	Model         string         `json:"model"`
	Stream        bool           `json:"stream"`
	Concurrency   int            `json:"concurrency"`
	Duration      string         `json:"duration"`
	Warmup        string         `json:"warmup"`
	Requests      int            `json:"requests"`
	OK            int            `json:"ok"`
	Errors        map[string]int `json:"errors"`
	ThroughputRPS float64        `json:"throughput_rps"`
	Latency       summary        `json:"latency_ms"`
	TTFB          summary        `json:"ttfb_ms"`
	StartedAt     time.Time      `json:"started_at"`
	GoVersion     string         `json:"go_version"`
	GOMAXPROCS    int            `json:"gomaxprocs"`
}

type sample struct {
	latency, ttfb time.Duration
	outcome       string // "ok", an HTTP status, or "transport"
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "compare" {
		return compare(args[1:], stdout)
	}
	flags := flag.NewFlagSet("llmgate-bench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	url := flags.String("url", "", "chat completions URL")
	model := flags.String("model", "bench;tokens=16", "model to ask for")
	stream := flags.Bool("stream", false, "ask for a streamed response")
	concurrency := flags.Int("concurrency", 8, "requests in flight at once")
	duration := flags.Duration("duration", 10*time.Second, "how long to measure")
	warmup := flags.Duration("warmup", 2*time.Second, "how long to send requests before measuring")
	label := flags.String("label", "", "a name for this run")
	cache := flags.Bool("cache", false, "opt in to the gateway's cache")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *url == "" || *concurrency < 1 || *duration <= 0 || *warmup < 0 {
		return errors.New("-url is required, and -concurrency and -duration must be positive")
	}

	body, err := json.Marshal(map[string]any{
		"model":    *model,
		"stream":   *stream,
		"messages": []map[string]string{{"role": "user", "content": "Reply with a short benchmark answer."}},
	})
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: *concurrency, MaxIdleConnsPerHost: *concurrency, DisableCompression: true,
	}}
	defer client.CloseIdleConnections()
	key := os.Getenv(keyEnv)

	start := time.Now()
	measureFrom, stop := start.Add(*warmup), start.Add(*warmup+*duration)
	var mu sync.Mutex
	var samples []sample
	var wg sync.WaitGroup
	for range *concurrency {
		wg.Go(func() {
			for {
				began := time.Now()
				if !began.Before(stop) || ctx.Err() != nil {
					return
				}
				s := send(ctx, client, *url, key, body, *cache)
				if began.Before(measureFrom) {
					continue
				}
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	r := result{
		Label: *label, URL: *url, Model: *model, Stream: *stream, Concurrency: *concurrency,
		Duration: duration.String(), Warmup: warmup.String(), Errors: map[string]int{},
		StartedAt: start.UTC(), GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
	var latencies, ttfbs []time.Duration
	for _, s := range samples {
		r.Requests++
		if s.outcome != "ok" {
			r.Errors[s.outcome]++
			continue
		}
		r.OK++
		latencies = append(latencies, s.latency)
		ttfbs = append(ttfbs, s.ttfb)
	}
	r.ThroughputRPS = float64(r.OK) / duration.Seconds()
	r.Latency, r.TTFB = summarize(latencies), summarize(ttfbs)

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "%s: %d requests, %d ok, %.0f/s, p50 %.2fms p99 %.2fms, ttfb p50 %.2fms\n",
		r.Label, r.Requests, r.OK, r.ThroughputRPS, r.Latency.P50, r.Latency.P99, r.TTFB.P50)
	return nil
}

// send makes one request and reads the whole response. Time to first byte is when the
// first byte of the body arrives, which for a stream is its first event.
func send(ctx context.Context, client *http.Client, url, key string, body []byte, cache bool) sample {
	began := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sample{outcome: "transport"}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if cache {
		req.Header.Set("X-Llmgate-Cache", "allow")
	}
	resp, err := client.Do(req)
	if err != nil {
		return sample{outcome: "transport"}
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 1)
	_, err = io.ReadFull(resp.Body, first)
	ttfb := time.Since(began)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return sample{outcome: strconv.Itoa(resp.StatusCode)}
	}
	if err != nil {
		return sample{outcome: "transport"}
	}
	return sample{latency: time.Since(began), ttfb: ttfb, outcome: "ok"}
}

// summarize returns nearest-rank percentiles in milliseconds.
func summarize(d []time.Duration) summary {
	if len(d) == 0 {
		return summary{}
	}
	sorted := slices.Clone(d)
	slices.Sort(sorted)
	rank := func(p float64) float64 {
		i := int(p*float64(len(sorted))+0.999999) - 1
		return ms(sorted[max(0, min(i, len(sorted)-1))])
	}
	var total time.Duration
	for _, v := range sorted {
		total += v
	}
	return summary{P50: rank(0.50), P95: rank(0.95), P99: rank(0.99), Max: ms(sorted[len(sorted)-1]), Mean: ms(total / time.Duration(len(sorted)))}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// compare prints results as a markdown table, with what each run adds to the first one.
func compare(paths []string, stdout io.Writer) error {
	if len(paths) == 0 {
		return errors.New("compare needs result files, the baseline first")
	}
	results := make([]result, len(paths))
	for i, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // Paths come from the operator.
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &results[i]); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	base := results[0]
	var b strings.Builder
	b.WriteString("| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		errs := "none"
		if len(r.Errors) > 0 {
			keys := make([]string, 0, len(r.Errors))
			for k := range r.Errors {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = fmt.Sprintf("%s: %d", k, r.Errors[k])
			}
			errs = strings.Join(parts, ", ")
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %.0f | %.2f | %.2f | %.2f | %.2f | %.2f | %+.2f | %+.2f | %+.2f | %+.2f |\n",
			r.Label, r.Requests, r.OK, errs, r.ThroughputRPS, r.Latency.P50, r.Latency.P95, r.Latency.P99, r.TTFB.P50, r.TTFB.P95,
			r.Latency.P50-base.Latency.P50, r.Latency.P95-base.Latency.P95, r.Latency.P99-base.Latency.P99, r.TTFB.P50-base.TTFB.P50)
	}
	_, err := io.WriteString(stdout, b.String())
	return err
}
