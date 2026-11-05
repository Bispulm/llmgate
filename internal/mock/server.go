package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxRequestBytes = 64 << 20

// apiError is an error response, rendered in the wire format of the request.
type apiError struct {
	status     int
	message    string
	retryAfter string
}

// request is what a wire format extracts from a validated request.
type request struct {
	model        string
	stream       bool
	includeUsage bool
	maxOutput    int // 0 means no limit was sent
	promptTokens int
}

// response is the scripted result of a request.
type response struct {
	scenario     scenario
	tokens       int  // text events in the response
	thinking     int  // thinking tokens, which also count against the output limit
	limited      bool // stopped by the output limit
	promptTokens int
	includeUsage bool
}

// outputTokens is the billable output, which includes thinking.
func (r response) outputTokens() int { return r.tokens + r.thinking }

func (r response) text() string {
	var b strings.Builder
	for i := range r.tokens {
		b.WriteString(r.scenario.delta(i))
	}
	return b.String()
}

// streamParts are the frames of a stream, separated by where failures can occur.
// Text frames are built one at a time, so that a long or heavily padded stream
// costs the mock almost no memory and can't distort a measurement.
type streamParts struct {
	prelude    [][]byte
	textFrame  func(i int) []byte
	epilogue   [][]byte
	terminator [][]byte
}

// wireFormat is one provider's request rules and response encoding.
type wireFormat interface {
	parse(r *http.Request, body []byte) (request, *apiError)
	writeError(w http.ResponseWriter, e *apiError)
	completion(resp response) []byte
	stream(resp response) streamParts
	streamErrorFrame(sc scenario) []byte
	malformedFrame(sc scenario) []byte
	oversizedFrame(sc scenario) []byte
}

// LabelStats counts what the mock saw for one model label.
type LabelStats struct {
	Requests     int64 `json:"requests"`
	Completed    int64 `json:"completed"`
	Canceled     int64 `json:"canceled"`
	BytesWritten int64 `json:"bytes_written"`
}

type labelCounters struct {
	requests, completed, canceled, bytesWritten atomic.Int64
}

// Server is the mock provider. Use New to create one.
type Server struct {
	mux *http.ServeMux

	mu        sync.Mutex
	labels    map[string]*labelCounters
	scenarios map[string]int // requests seen per exact model string
}

// New returns a mock provider serving POST /v1/chat/completions,
// POST /v1/messages, and GET /__mock/stats.
func New() *Server {
	s := &Server{mux: http.NewServeMux(), labels: make(map[string]*labelCounters), scenarios: make(map[string]int)}
	s.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, openAIWire{})
	})
	s.mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, anthropicWire{})
	})
	s.mux.HandleFunc("GET /__mock/stats", s.serveStats)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Stats returns a snapshot of the counters for every label seen.
func (s *Server) Stats() map[string]LabelStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]LabelStats, len(s.labels))
	for label, c := range s.labels {
		out[label] = LabelStats{
			Requests:     c.requests.Load(),
			Completed:    c.completed.Load(),
			Canceled:     c.canceled.Load(),
			BytesWritten: c.bytesWritten.Load(),
		}
	}
	return out
}

func (s *Server) serveStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Stats())
}

// countScenario returns how many requests have used this exact model string,
// including this one.
func (s *Server) countScenario(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios[model]++
	return s.scenarios[model]
}

func (s *Server) counters(label string) *labelCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.labels[label]
	if !ok {
		c = &labelCounters{}
		s.labels[label] = c
	}
	return c
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, wire wireFormat) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		wire.writeError(w, &apiError{status: http.StatusBadRequest, message: "could not read the request body"})
		return
	}
	req, apiErr := wire.parse(r, body)
	if apiErr != nil {
		wire.writeError(w, apiErr)
		return
	}
	sc, err := parseScenario(req.model)
	if err != nil {
		wire.writeError(w, &apiError{status: http.StatusBadRequest, message: "invalid mock scenario: " + err.Error()})
		return
	}
	if sc.fail == failStreamError && !req.stream {
		wire.writeError(w, &apiError{status: http.StatusBadRequest, message: "stream_error needs a streaming request"})
		return
	}

	counters := s.counters(sc.label)
	counters.requests.Add(1)
	if sc.failFirst > 0 && s.countScenario(req.model) > sc.failFirst {
		sc.fail = ""
	}
	out := &output{w: w, ctl: http.NewResponseController(w), chunk: sc.chunk, counters: counters}

	if err := sleep(r.Context(), sc.latency); err != nil {
		counters.canceled.Add(1)
		return
	}

	switch sc.fail {
	case failStatus:
		wire.writeError(w, &apiError{status: sc.status, message: "mock failure", retryAfter: sc.retryAfter})
		counters.completed.Add(1)
		return
	case failDropBeforeHeaders:
		abortConnection(w)
		counters.completed.Add(1)
		return
	}

	// Thinking comes first, so it uses up the limit before any text.
	thinking, n := sc.thinking, sc.tokens
	limited := req.maxOutput > 0 && thinking+n > req.maxOutput
	if limited {
		thinking = min(thinking, req.maxOutput)
		n = req.maxOutput - thinking
	}
	resp := response{
		scenario:     sc,
		tokens:       n,
		thinking:     thinking,
		limited:      limited,
		promptTokens: req.promptTokens,
		includeUsage: req.includeUsage,
	}

	if req.stream {
		err = serveStream(r.Context(), w, out, wire, resp)
	} else {
		err = serveCompletion(w, out, wire, resp)
	}
	if err != nil {
		counters.canceled.Add(1)
		return
	}
	counters.completed.Add(1)
}

func serveCompletion(w http.ResponseWriter, out *output, wire wireFormat, resp response) error {
	sc := resp.scenario
	body := wire.completion(resp)

	switch sc.fail {
	case failEOFBeforeFirstEvent:
		body = nil
	case failDisconnectBeforeFirst:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := out.flush(); err != nil {
			return err
		}
		abortConnection(w)
		return nil
	case failMalformed:
		body = body[:len(body)/2]
	case failOversized:
		body = append(body, bytes.Repeat([]byte(" "), sc.size)...)
	case failDisconnect:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := out.write(body[:len(body)/2]); err != nil {
			return err
		}
		abortConnection(w)
		return nil
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return out.write(body)
}

func serveStream(ctx context.Context, w http.ResponseWriter, out *output, wire wireFormat, resp response) error {
	sc := resp.scenario
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := out.flush(); err != nil {
		return err
	}

	switch sc.fail {
	case failEOFBeforeFirstEvent:
		return nil
	case failDisconnectBeforeFirst:
		abortConnection(w)
		return nil
	}

	if err := sleep(ctx, sc.ttfb); err != nil {
		return err
	}

	parts := wire.stream(resp)
	if err := out.writeAll(parts.prelude); err != nil {
		return err
	}
	for i := range resp.tokens {
		if i == sc.after {
			if handled, err := injectFailure(w, out, wire, sc); handled {
				return err
			}
		}
		if err := out.write(parts.textFrame(i)); err != nil {
			return err
		}
		if i < resp.tokens-1 {
			if err := sleep(ctx, sc.interval); err != nil {
				return err
			}
		}
	}
	if sc.after >= resp.tokens {
		if handled, err := injectFailure(w, out, wire, sc); handled {
			return err
		}
	}
	if err := out.writeAll(parts.epilogue); err != nil {
		return err
	}
	if sc.noEnd {
		return nil
	}
	return out.writeAll(parts.terminator)
}

// injectFailure applies a midstream failure, reporting whether one applied.
func injectFailure(w http.ResponseWriter, out *output, wire wireFormat, sc scenario) (bool, error) {
	switch sc.fail {
	case failDisconnect:
		abortConnection(w)
		return true, nil
	case failMalformed:
		return true, out.write(wire.malformedFrame(sc))
	case failOversized:
		return true, out.write(wire.oversizedFrame(sc))
	case failStreamError:
		return true, out.write(wire.streamErrorFrame(sc))
	}
	return false, nil
}

// abortConnection closes the connection with a reset, so the client sees a
// transport failure rather than a clean end of stream.
func abortConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("canceled while waiting: %w", ctx.Err())
	}
}

// output writes and flushes response bytes, in fragments when a scenario asks.
type output struct {
	w        http.ResponseWriter
	ctl      *http.ResponseController
	chunk    int
	counters *labelCounters
}

func (o *output) writeAll(frames [][]byte) error {
	for _, frame := range frames {
		if err := o.write(frame); err != nil {
			return err
		}
	}
	return nil
}

func (o *output) write(b []byte) error {
	size := o.chunk
	if size <= 0 {
		size = max(len(b), 1)
	}
	for len(b) > 0 {
		n := min(size, len(b))
		if _, err := o.w.Write(b[:n]); err != nil {
			return fmt.Errorf("writing response: %w", err)
		}
		if err := o.flush(); err != nil {
			return err
		}
		o.counters.bytesWritten.Add(int64(n))
		b = b[n:]
	}
	return nil
}

func (o *output) flush() error {
	if err := o.ctl.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flushing response: %w", err)
	}
	return nil
}

// decodeStrict decodes a body, rejecting fields a provider would not accept.
func decodeStrict(body []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// countPromptTokens is a deterministic stand-in for a tokenizer.
func countPromptTokens(texts ...string) int {
	n := 0
	for _, text := range texts {
		n += len(strings.Fields(text)) + 4
	}
	return n
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mock: encoding response: %v", err))
	}
	return b
}
