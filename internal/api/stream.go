package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/sse"
)

const slogLevelDebug = slog.LevelDebug

func attrRequestID(id string) slog.Attr { return slog.String("request_id", id) }

var doneEvent = []byte("data: [DONE]\n\n")

// streamWriter writes the client's event stream. It writes and flushes one event
// at a time and holds nothing else, so a slow client slows the upstream read
// instead of growing a queue here.
type streamWriter struct {
	x      *exchange
	server *Server
	usage  json.RawMessage // what non-final chunks carry in their usage field

	// encoded and enc are reused for every event of the stream.
	encoded bytes.Buffer
	enc     *json.Encoder
	out     []byte
}

func newStreamWriter(x *exchange, server *Server) *streamWriter {
	sw := &streamWriter{x: x, server: server}
	if x.req.IncludeUsage {
		sw.usage = json.RawMessage("null")
	}
	sw.enc = json.NewEncoder(&sw.encoded)
	sw.enc.SetEscapeHTML(false)
	return sw
}

// commit sends the response headers, the role chunk, and the first event together.
func (sw *streamWriter) commit(first chat.StreamEvent) error {
	h := sw.x.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Asks intermediaries such as nginx not to buffer the stream.
	h.Set("X-Accel-Buffering", "no")
	sw.x.w.WriteHeader(http.StatusOK)

	empty := ""
	if err := sw.appendChunk(wireDelta{Role: roleAssistant, Content: &empty}, nil); err != nil {
		return err
	}
	if err := sw.appendEvent(first); err != nil {
		return err
	}
	return sw.flush()
}

func (sw *streamWriter) write(event chat.StreamEvent) error {
	if err := sw.appendEvent(event); err != nil {
		return err
	}
	return sw.flush()
}

// finish ends a successful stream. Usage comes here, however early the provider
// reported it, because the contract places it immediately before [DONE].
func (sw *streamWriter) finish(usage *chat.Usage) error {
	if wire := newWireUsage(usage); wire != nil && sw.x.req.IncludeUsage {
		raw, err := encodeJSON(wire)
		if err != nil {
			return err
		}
		if err := sw.appendJSON(wireChunk{
			ID:      completionPrefix + sw.x.id,
			Object:  objectChunk,
			Created: sw.x.created,
			Model:   sw.x.alias,
			Choices: []wireChunkChoice{},
			Usage:   raw,
		}); err != nil {
			return err
		}
	}
	sw.out = append(sw.out, doneEvent...)
	return sw.flush()
}

// writeError sends one error event. The stream then ends with no [DONE], which is
// how a client knows it failed.
func (sw *streamWriter) writeError(e *apiError) error {
	if err := sw.appendJSON(e.envelope()); err != nil {
		return err
	}
	return sw.flush()
}

func (sw *streamWriter) appendEvent(event chat.StreamEvent) error {
	if event.Content != "" {
		if err := sw.appendChunk(wireDelta{Content: &event.Content}, nil); err != nil {
			return err
		}
	}
	if event.Refusal != "" {
		if err := sw.appendChunk(wireDelta{Refusal: &event.Refusal}, nil); err != nil {
			return err
		}
	}
	if event.FinishReason != "" {
		reason := string(event.FinishReason)
		if err := sw.appendChunk(wireDelta{}, &reason); err != nil {
			return err
		}
	}
	return nil
}

func (sw *streamWriter) appendChunk(delta wireDelta, finish *string) error {
	return sw.appendJSON(wireChunk{
		ID:      completionPrefix + sw.x.id,
		Object:  objectChunk,
		Created: sw.x.created,
		Model:   sw.x.alias,
		Choices: []wireChunkChoice{{Delta: delta, FinishReason: finish}},
		Usage:   sw.usage,
	})
}

func (sw *streamWriter) appendJSON(v any) error {
	sw.encoded.Reset()
	if err := sw.enc.Encode(v); err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}
	sw.out = sse.AppendData(sw.out, bytes.TrimRight(sw.encoded.Bytes(), "\n"))
	return nil
}

func (sw *streamWriter) flush() error {
	sw.server.extendWriteDeadline(sw.x.rc)
	_, err := sw.x.w.Write(sw.out)
	sw.out = sw.out[:0]
	if err != nil {
		return fmt.Errorf("writing to the client: %w", err)
	}
	if err := sw.x.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flushing to the client: %w", err)
	}
	return nil
}
