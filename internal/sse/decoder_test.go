package sse

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const testLimit = 1 << 16

type decoded struct {
	Name, Data string
}

// decodeAll drains a decoder, copying each event because Event fields are only
// valid until the next call.
func decodeAll(r io.Reader, limit int) ([]decoded, error) {
	d := NewDecoder(r, limit)
	var out []decoded
	for {
		ev, err := d.Next()
		if err != nil {
			return out, err
		}
		out = append(out, decoded{string(ev.Name), string(ev.Data)})
	}
}

func TestDecoderParsesEventStreams(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []decoded
		wantErr error
	}{
		{name: "empty stream", input: "", wantErr: io.EOF},
		{
			name:    "single data event",
			input:   "data: hello\n\n",
			want:    []decoded{{"", "hello"}},
			wantErr: io.EOF,
		},
		{
			name:    "named event",
			input:   "event: message_stop\ndata: {}\n\n",
			want:    []decoded{{"message_stop", "{}"}},
			wantErr: io.EOF,
		},
		{
			name:    "multiline data joins with a newline",
			input:   "data: {\"a\":\ndata: 1}\n\n",
			want:    []decoded{{"", "{\"a\":\n1}"}},
			wantErr: io.EOF,
		},
		{
			name:    "comments are ignored",
			input:   ": keepalive\ndata: x\n: another\n\n",
			want:    []decoded{{"", "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "only one leading space is removed",
			input:   "data:a\n\ndata: b\n\ndata:  c\n\n",
			want:    []decoded{{"", "a"}, {"", "b"}, {"", " c"}},
			wantErr: io.EOF,
		},
		{
			name:    "a field with no colon has an empty value",
			input:   "data\n\n",
			want:    []decoded{{"", ""}},
			wantErr: io.EOF,
		},
		{
			name:    "an empty data line still dispatches",
			input:   "data:\n\n",
			want:    []decoded{{"", ""}},
			wantErr: io.EOF,
		},
		{
			name:    "unknown fields are ignored",
			input:   "id: 7\nretry: 100\nfoo: bar\ndata: x\n\n",
			want:    []decoded{{"", "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "CRLF line endings",
			input:   "event: a\r\ndata: x\r\n\r\ndata: y\r\n\r\n",
			want:    []decoded{{"a", "x"}, {"", "y"}},
			wantErr: io.EOF,
		},
		{
			name:    "CR line endings",
			input:   "data: x\r\rdata: y\r\r",
			want:    []decoded{{"", "x"}, {"", "y"}},
			wantErr: io.EOF,
		},
		{
			name:    "mixed line endings",
			input:   "data: x\r\n\ndata: y\n\r\ndata: z\r\r",
			want:    []decoded{{"", "x"}, {"", "y"}, {"", "z"}},
			wantErr: io.EOF,
		},
		{
			name:    "a leading byte order mark is removed",
			input:   "\xef\xbb\xbfdata: x\n\n",
			want:    []decoded{{"", "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "a blank line with no data dispatches nothing and resets the name",
			input:   "event: stale\n\n\n\ndata: x\n\n",
			want:    []decoded{{"", "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "a trailing comment ends cleanly",
			input:   "data: x\n\n: bye\n",
			want:    []decoded{{"", "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "EOF before the blank line is not a clean end",
			input:   "data: complete\n\ndata: partial\n",
			want:    []decoded{{"", "complete"}},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "EOF inside a line is not a clean end",
			input:   "data: complete\n\ndata: par",
			want:    []decoded{{"", "complete"}},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "EOF after only an event name is not a clean end",
			input:   "event: x\n",
			wantErr: io.ErrUnexpectedEOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeAll(strings.NewReader(tt.input), testLimit)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("events = %q, want %q", got, tt.want)
			}
		})
	}
}

// A network read can end anywhere, so no split of the input may change the
// result. This covers events split across reads, CRLF split between the CR and
// the LF, and a multi-byte character split between its bytes.
func TestDecoderResultIndependentOfReadBoundaries(t *testing.T) {
	stream := "\xef\xbb\xbfevent: a\r\ndata: héllo 世界 🙂\r\n: comment\r\n\r\n" +
		"data: {\"a\":\ndata: 1}\n\n" +
		"data: x\r\rdata: y\n\n"

	want, wantErr := decodeAll(strings.NewReader(stream), testLimit)
	if !errors.Is(wantErr, io.EOF) {
		t.Fatalf("baseline error = %v", wantErr)
	}
	if len(want) != 4 {
		t.Fatalf("baseline events = %q, want 4", want)
	}

	check := func(t *testing.T, r io.Reader) {
		t.Helper()
		got, err := decodeAll(r, testLimit)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %q, want %q", got, want)
		}
	}

	t.Run("every two-part split", func(t *testing.T) {
		for i := range len(stream) + 1 {
			check(t, io.MultiReader(strings.NewReader(stream[:i]), strings.NewReader(stream[i:])))
		}
	})
	t.Run("one byte at a time", func(t *testing.T) {
		check(t, iotest.OneByteReader(strings.NewReader(stream)))
	})
	t.Run("half reads", func(t *testing.T) {
		check(t, iotest.HalfReader(strings.NewReader(stream)))
	})
	t.Run("data returned together with EOF", func(t *testing.T) {
		check(t, iotest.DataErrReader(strings.NewReader(stream)))
	})
}

// scriptedReader returns each chunk from one Read, then its final error.
type scriptedReader struct {
	chunks [][]byte
	final  error
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, r.final
	}
	n := copy(p, r.chunks[0])
	if n == len(r.chunks[0]) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = r.chunks[0][n:]
	}
	return n, nil
}

// Reading past a complete event would stall delivery until the upstream sends
// more, which would add latency to every streamed token.
func TestDecoderDispatchesWithoutReadingAhead(t *testing.T) {
	errBlocked := errors.New("decoder read past a complete event")

	for _, input := range []string{"data: a\n\n", "data: a\r\n\r\n", "data: a\r\r"} {
		d := NewDecoder(&scriptedReader{chunks: [][]byte{[]byte(input)}, final: errBlocked}, testLimit)
		ev, err := d.Next()
		if err != nil {
			t.Fatalf("input %q: %v", input, err)
		}
		if string(ev.Data) != "a" {
			t.Fatalf("input %q: data = %q", input, ev.Data)
		}
	}
}

func TestDecoderDoesNotCountCRLFAcrossReadsAsTwoLineEnds(t *testing.T) {
	r := &scriptedReader{
		chunks: [][]byte{[]byte("data: a\r"), []byte("\n\r"), []byte("\ndata: b\r\n\r\n")},
		final:  io.EOF,
	}
	got, err := decodeAll(r, testLimit)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v", err)
	}
	want := []decoded{{"", "a"}, {"", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestDecoderPreservesFragmentedUTF8(t *testing.T) {
	text := "héllo 世界 🙂"
	stream := "data: " + text + "\n\n"
	for i := range len(stream) + 1 {
		got, err := decodeAll(io.MultiReader(strings.NewReader(stream[:i]), strings.NewReader(stream[i:])), testLimit)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("split %d: %v", i, err)
		}
		if len(got) != 1 || got[0].Data != text {
			t.Fatalf("split %d: events = %q", i, got)
		}
	}
}

func TestDecoderReturnsReaderErrors(t *testing.T) {
	errReset := errors.New("connection reset")

	t.Run("between events", func(t *testing.T) {
		got, err := decodeAll(&scriptedReader{chunks: [][]byte{[]byte("data: a\n\n")}, final: errReset}, testLimit)
		if !errors.Is(err, errReset) {
			t.Fatalf("error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("events = %q", got)
		}
	})
	t.Run("inside an event", func(t *testing.T) {
		_, err := decodeAll(&scriptedReader{chunks: [][]byte{[]byte("data: par")}, final: errReset}, testLimit)
		if !errors.Is(err, errReset) {
			t.Fatalf("error = %v", err)
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

func TestDecoderGivesUpOnAReaderMakingNoProgress(t *testing.T) {
	_, err := NewDecoder(zeroReader{}, testLimit).Next()
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("error = %v", err)
	}
}

func TestDecoderEnforcesEventLimit(t *testing.T) {
	const limit = 64
	prefix := "data: "

	line := func(total int) string { return prefix + strings.Repeat("x", total-len(prefix)) }

	t.Run("a line at the limit passes", func(t *testing.T) {
		got, err := decodeAll(strings.NewReader(line(limit)+"\n\n"), limit)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("error = %v", err)
		}
		if len(got) != 1 || len(got[0].Data) != limit-len(prefix) {
			t.Fatalf("events = %q", got)
		}
	})
	t.Run("a line one byte over fails", func(t *testing.T) {
		_, err := decodeAll(strings.NewReader(line(limit+1)+"\n\n"), limit)
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("several lines count together", func(t *testing.T) {
		_, err := decodeAll(strings.NewReader(line(40)+"\n"+line(40)+"\n\n"), limit)
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("comments count, so a comment flood cannot grow without bound", func(t *testing.T) {
		_, err := decodeAll(strings.NewReader(strings.Repeat(": 0123456789\n", 20)), limit)
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("the limit resets after each event", func(t *testing.T) {
		got, err := decodeAll(strings.NewReader(strings.Repeat(line(limit)+"\n\n", 5)), limit)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("error = %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("got %d events, want 5", len(got))
		}
	})
	t.Run("an unterminated line fails without reading all of it", func(t *testing.T) {
		src := bytes.NewReader(bytes.Repeat([]byte("x"), 10*limit))
		_, err := decodeAll(src, limit)
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
		if consumed := int(src.Size()) - src.Len(); consumed > 2*limit {
			t.Fatalf("consumed %d bytes with a limit of %d", consumed, limit)
		}
	})
	t.Run("errors are terminal", func(t *testing.T) {
		d := NewDecoder(strings.NewReader(line(limit+1)+"\n\ndata: later\n\n"), limit)
		for range 2 {
			if _, err := d.Next(); !errors.Is(err, ErrEventTooLarge) {
				t.Fatalf("error = %v", err)
			}
		}
	})
}

func TestDecoderBufferStaysWithinTheLimit(t *testing.T) {
	const limit = 1 << 20
	d := NewDecoder(strings.NewReader("data: "+strings.Repeat("x", 10*limit)+"\n\n"), limit)
	if _, err := d.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if got := cap(d.buf); got > limit+1 {
		t.Fatalf("buffer capacity = %d, want at most %d", got, limit+1)
	}
}

// With one byte per read, rescanning all unread input on each read would be
// quadratic in the event limit, letting a slow upstream burn CPU. Linear scanning
// takes well under a second here, and quadratic scanning takes minutes, so the
// bound is generous enough to hold on a loaded machine.
func TestDecoderScanningIsLinearWithOneByteReads(t *testing.T) {
	const size = 1 << 20
	input := "data: " + strings.Repeat("x", size) + "\n\n"
	d := NewDecoder(iotest.OneByteReader(strings.NewReader(input)), 2*size)

	start := time.Now()
	ev, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Data) != size {
		t.Fatalf("data length = %d", len(ev.Data))
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("decoding took %v, which suggests quadratic scanning", elapsed)
	}
}

func TestDecoderAllocatesNothingPerEventOnceWarm(t *testing.T) {
	event := []byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello world\"}}\n\n")
	src := &loopReader{chunk: event}
	d := NewDecoder(src, testLimit)
	if _, err := d.Next(); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if _, err := d.Next(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("allocations per event = %v, want 0", allocs)
	}
}

// loopReader returns the same chunk forever.
type loopReader struct {
	chunk []byte
}

func (r *loopReader) Read(p []byte) (int, error) { return copy(p, r.chunk), nil }

func BenchmarkDecoder(b *testing.B) {
	event := []byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
	d := NewDecoder(&loopReader{chunk: event}, testLimit)
	b.SetBytes(int64(len(event)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := d.Next(); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzDecoderIgnoresReadBoundaries(f *testing.F) {
	f.Add([]byte("data: x\n\n"), 1)
	f.Add([]byte("event: a\r\ndata: b\r\n\r\n"), 3)
	f.Add([]byte("\xef\xbb\xbfdata:\r\r: c\ndata\n\n"), 2)
	f.Add([]byte("data: \xff\xfe\n\ndata: par"), 5)

	f.Fuzz(func(t *testing.T, input []byte, chunk int) {
		if chunk < 1 {
			chunk = 1
		}
		const limit = 1 << 12

		whole, wholeErr := decodeAll(bytes.NewReader(input), limit)
		var chunks [][]byte
		for rest := input; len(rest) > 0; {
			n := min(chunk, len(rest))
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		split, splitErr := decodeAll(&scriptedReader{chunks: chunks, final: io.EOF}, limit)

		if !errors.Is(splitErr, wholeErr) {
			t.Fatalf("errors differ: whole %v, split %v", wholeErr, splitErr)
		}
		if !reflect.DeepEqual(whole, split) {
			t.Fatalf("events differ: whole %q, split %q", whole, split)
		}
		for _, ev := range whole {
			if len(ev.Name)+len(ev.Data) > limit {
				t.Fatalf("event exceeds limit: %d bytes", len(ev.Name)+len(ev.Data))
			}
		}
	})
}
