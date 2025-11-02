package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// A request's trace shows each attempt and each ledger step, under one parent, with what
// routing and accounting decided and nothing of what the client sent.
func TestARequestIsTracedWithoutItsContent(t *testing.T) {
	const failing, working = "primary;fail=status;status=500", "backup;tokens=2"
	e := newTenantEnv(t, failing, working)
	key := e.addTeam("a", 100_000)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })
	url, client := e.instance(instanceOptions{tracer: provider.Tracer("test")}, failing, working)

	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// A caller's trace context is continued.
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var spans []sdktrace.ReadOnlySpan
	deadline := time.Now().Add(5 * time.Second)
	for {
		spans = recorder.Ended()
		if len(spans) > 0 && spans[len(spans)-1].Name() == "chat.completions" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = append(byName[s.Name()], s)
	}
	root := byName["chat.completions"]
	if len(root) != 1 {
		t.Fatalf("spans = %v", byName)
	}
	if got := root[0].SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the caller's trace wasn't continued: %s", got)
	}
	attrs := spanAttrs(root[0])
	if attrs["llmgate.alias"].AsString() != "fast" || attrs["http.response.status_code"].AsInt64() != 200 || attrs["llmgate.attempts"].AsInt64() != 2 {
		t.Fatalf("request attributes = %v", attrs)
	}

	attempts := byName["attempt"]
	if len(attempts) != 2 {
		t.Fatalf("%d attempt spans", len(attempts))
	}
	for _, a := range attempts {
		if a.Parent().SpanID() != root[0].SpanContext().SpanID() {
			t.Fatal("an attempt isn't under its request")
		}
	}
	first, second := spanAttrs(attempts[0]), spanAttrs(attempts[1])
	if first["llmgate.attempt.number"].AsInt64() != 1 || first["llmgate.outcome"].AsString() != "failed" || first["llmgate.error_kind"].AsString() != "server" {
		t.Fatalf("first attempt = %v", first)
	}
	if second["llmgate.attempt.reason"].AsString() != "fallback" || second["llmgate.charge_kind"].AsString() != "reported" {
		t.Fatalf("second attempt = %v", second)
	}
	for _, name := range []string{"ledger.reserve", "ledger.mark_dispatched", "ledger.finish"} {
		if len(byName[name]) != 2 {
			t.Fatalf("%d %s spans", len(byName[name]), name)
		}
	}

	for _, s := range spans {
		for _, kv := range s.Attributes() {
			for _, secret := range []string{testPrompt, key, testProviderKey} {
				if strings.Contains(kv.Value.String(), secret) {
					t.Fatalf("span %s attribute %s holds %q", s.Name(), kv.Key, secret)
				}
			}
		}
	}
}

// Anyone can send a trace header, so nothing is traced until a caller has authenticated.
// Otherwise unauthenticated traffic could force spans to be exported.
func TestNothingIsTracedBeforeAuthentication(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })
	g := newGateway(t, "alpha;tokens=1", func(o *Options) { o.Tracer = provider.Tracer("test") })

	req, err := http.NewRequest(http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := len(recorder.Ended()) + len(recorder.Started()); got != 0 {
		t.Fatalf("%d spans for an unauthenticated request", got)
	}
}
