// Package sse reads and writes server-sent events.
//
// The decoder follows the WHATWG event stream format, with two deliberate
// differences that matter to a proxy: every event is bounded in size, and a
// stream that ends inside an event is an error rather than being discarded
// silently.
package sse

import (
	"bytes"
	"errors"
	"io"
)

// ErrEventTooLarge reports that an event exceeded the decoder's size limit.
var ErrEventTooLarge = errors.New("sse: event exceeds size limit")

const (
	initialBufferSize = 4 << 10
	maxEmptyReads     = 100
)

var byteOrderMark = []byte("\xef\xbb\xbf")

// Event is one dispatched event. Its fields are only valid until the next call
// to Next, which lets a long stream decode without allocating per event.
type Event struct {
	Name []byte
	Data []byte
}

// Decoder reads events incrementally. It returns each event as soon as its
// terminating blank line arrives and never reads ahead for more.
//
// It holds at most about maxEventBytes of input, however the stream behaves.
type Decoder struct {
	r   io.Reader
	max int

	buf        []byte
	start, end int
	scanned    int // unread bytes already known to hold no line ending

	name, data []byte
	hasData    bool
	size       int // bytes of lines consumed since the last dispatch

	skipLF     bool // the last line ended in CR, so a following LF belongs to it
	bomChecked bool

	readErr error // the reader's error, surfaced once buffered input runs out
	err     error // terminal: returned by every later call
}

// NewDecoder returns a decoder that fails with ErrEventTooLarge once the lines
// of one event, comments included and line endings excluded, exceed
// maxEventBytes. maxEventBytes must be positive.
func NewDecoder(r io.Reader, maxEventBytes int) *Decoder {
	return &Decoder{
		r:   r,
		max: maxEventBytes,
		buf: make([]byte, min(initialBufferSize, maxEventBytes+1)),
	}
}

// Next returns the next event.
//
// At the end of input it returns io.EOF if the stream ended between events and
// io.ErrUnexpectedEOF if it ended inside one. A reader failure is returned
// unchanged. Errors are terminal.
func (d *Decoder) Next() (Event, error) {
	if d.err != nil {
		return Event{}, d.err
	}
	d.name, d.data = d.name[:0], d.data[:0]
	d.hasData = false
	d.size = 0

	for {
		line, err := d.readLine()
		if err != nil {
			d.err = err
			return Event{}, err
		}
		if len(line) == 0 {
			if d.hasData {
				return Event{Name: d.name, Data: d.data}, nil
			}
			d.name = d.name[:0]
			continue
		}
		d.size += len(line)
		d.processLine(line)
	}
}

// readLine returns the next line without its ending. The result aliases the
// internal buffer and is valid until the next read.
func (d *Decoder) readLine() ([]byte, error) {
	for {
		if !d.bomChecked {
			if d.end-d.start < len(byteOrderMark) && d.readErr == nil {
				d.fill()
				continue
			}
			if bytes.HasPrefix(d.buf[d.start:d.end], byteOrderMark) {
				d.start += len(byteOrderMark)
			}
			d.bomChecked = true
		}

		if d.skipLF && d.start < d.end {
			if d.buf[d.start] == '\n' {
				d.start++
			}
			d.skipLF = false
		}

		unread := d.buf[d.start:d.end]
		if i := bytes.IndexAny(unread[d.scanned:], "\r\n"); i >= 0 {
			i += d.scanned
			if d.size+i > d.max {
				return nil, ErrEventTooLarge
			}
			d.skipLF = unread[i] == '\r'
			d.start += i + 1
			d.scanned = 0
			return unread[:i], nil
		}
		d.scanned = len(unread)

		if d.size+len(unread) > d.max {
			return nil, ErrEventTooLarge
		}
		if d.readErr != nil {
			return nil, d.endOfInput(len(unread) > 0)
		}
		d.fill()
	}
}

func (d *Decoder) endOfInput(insideLine bool) error {
	if !errors.Is(d.readErr, io.EOF) {
		return d.readErr
	}
	if insideLine || d.hasData || len(d.name) > 0 {
		return io.ErrUnexpectedEOF
	}
	return io.EOF
}

func (d *Decoder) processLine(line []byte) {
	field, value := line, []byte(nil)
	if i := bytes.IndexByte(line, ':'); i == 0 {
		return // comment
	} else if i > 0 {
		field, value = line[:i], line[i+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}

	switch string(field) {
	case "data":
		if d.hasData {
			d.data = append(d.data, '\n')
		}
		d.data = append(d.data, value...)
		d.hasData = true
	case "event":
		d.name = append(d.name[:0], value...)
	}
}

// fill makes room and performs one read, recording any error for later so that
// data returned alongside it is processed first.
func (d *Decoder) fill() {
	if d.start > 0 {
		d.end = copy(d.buf, d.buf[d.start:d.end])
		d.start = 0
	}
	if d.end == len(d.buf) {
		grown := make([]byte, min(2*len(d.buf), d.max+1))
		copy(grown, d.buf[:d.end])
		d.buf = grown
	}
	for range maxEmptyReads {
		n, err := d.r.Read(d.buf[d.end:])
		d.end += n
		if err != nil {
			d.readErr = err
			return
		}
		if n > 0 {
			return
		}
	}
	d.readErr = io.ErrNoProgress
}
