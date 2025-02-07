// Package mock is a deterministic local stand-in for LLM providers. It speaks
// both supported wire formats over real HTTP and injects scripted failures, so
// no default test or demo needs a paid API.
//
// It shares no types with the gateway on purpose. A misunderstanding of a wire
// format can't be hidden by both sides using the same struct.
//
// A scenario is selected by the upstream model string:
//
//	label[;key=value...]
//
// The label names the model and appears in its response text, which is how a
// test proves which route served a request. Unknown keys and values are
// rejected, so a typo can't silently run the success path.
package mock

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Failure modes selected by the fail key.
const (
	failStatus                = "status"
	failDropBeforeHeaders     = "drop_before_headers"
	failEOFBeforeFirstEvent   = "eof_before_first_event"
	failDisconnectBeforeFirst = "disconnect_before_first_event"
	failDisconnect            = "disconnect"
	failMalformed             = "malformed"
	failOversized             = "oversized"
	failStreamError           = "stream_error"
)

const (
	usageReported     = "reported"
	usageNone         = "none"
	usageInconsistent = "inconsistent"

	textASCII   = "ascii"
	textUnicode = "unicode"

	defaultTokens       = 8
	defaultOversizeSize = 2 << 20
)

type scenario struct {
	label      string
	fail       string
	status     int
	retryAfter string
	after      int // text events delivered before a midstream failure

	latency  time.Duration // before response headers
	ttfb     time.Duration // between response headers and the first event
	interval time.Duration // between text events

	tokens int
	pad    int // extra bytes added to each text event
	size   int // size of an oversized event

	chunk     int    // write size in bytes, each write flushed; 0 writes whole events
	usage     string // usageReported, usageNone, or usageInconsistent
	noEnd     bool   // omit [DONE] or message_stop
	eol       string
	comments  bool
	multiline bool
	text      string
}

func parseScenario(model string) (scenario, error) {
	parts := strings.Split(model, ";")
	sc := scenario{
		label:  parts[0],
		tokens: defaultTokens,
		size:   defaultOversizeSize,
		usage:  usageReported,
		eol:    "\n",
		text:   textASCII,
	}
	if sc.label == "" {
		return sc, fmt.Errorf("model has no label")
	}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return sc, fmt.Errorf("option %q is not key=value", part)
		}
		if err := sc.set(key, value); err != nil {
			return sc, fmt.Errorf("option %q: %w", key, err)
		}
	}
	if sc.fail == failStatus && (sc.status < 400 || sc.status > 599) {
		return sc, fmt.Errorf("fail=status needs a status from 400 to 599")
	}
	return sc, nil
}

func (sc *scenario) set(key, value string) error {
	var err error
	switch key {
	case "fail":
		switch value {
		case failStatus, failDropBeforeHeaders, failEOFBeforeFirstEvent, failDisconnectBeforeFirst,
			failDisconnect, failMalformed, failOversized, failStreamError:
			sc.fail = value
		default:
			return fmt.Errorf("unknown failure %q", value)
		}
	case "status":
		sc.status, err = parseCount(value)
	case "retry_after":
		sc.retryAfter = value
	case "after":
		sc.after, err = parseCount(value)
	case "latency":
		sc.latency, err = time.ParseDuration(value)
	case "ttfb":
		sc.ttfb, err = time.ParseDuration(value)
	case "interval":
		sc.interval, err = time.ParseDuration(value)
	case "tokens":
		sc.tokens, err = parseCount(value)
	case "pad":
		sc.pad, err = parseCount(value)
	case "size":
		sc.size, err = parseCount(value)
	case "chunk":
		sc.chunk, err = parseCount(value)
	case "usage":
		switch value {
		case usageReported, usageNone, usageInconsistent:
			sc.usage = value
		default:
			return fmt.Errorf("unknown usage mode %q", value)
		}
	case "terminator":
		switch value {
		case "normal":
			sc.noEnd = false
		case "none":
			sc.noEnd = true
		default:
			return fmt.Errorf("unknown terminator %q", value)
		}
	case "eol":
		switch value {
		case "lf":
			sc.eol = "\n"
		case "crlf":
			sc.eol = "\r\n"
		case "cr":
			sc.eol = "\r"
		default:
			return fmt.Errorf("unknown line ending %q", value)
		}
	case "comments":
		sc.comments, err = strconv.ParseBool(value)
	case "multiline":
		sc.multiline, err = strconv.ParseBool(value)
	case "text":
		switch value {
		case textASCII, textUnicode:
			sc.text = value
		default:
			return fmt.Errorf("unknown text mode %q", value)
		}
	default:
		return fmt.Errorf("unknown option")
	}
	return err
}

func parseCount(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return n, nil
}

// delta returns the text of event i. Each event counts as one token, which keeps
// usage exact and predictable in tests.
func (sc scenario) delta(i int) string {
	var text string
	switch {
	case i == 0:
		text = "[" + sc.label + "]"
	case sc.text == textUnicode:
		words := [...]string{" héllo", " 世界", " 🙂"}
		text = words[(i-1)%len(words)]
	default:
		text = " token-" + strconv.Itoa(i)
	}
	if sc.pad > 0 {
		text += strings.Repeat("x", sc.pad)
	}
	return text
}

// frame formats one event using the scenario's framing options.
func (sc scenario) frame(name string, data []byte) []byte {
	out := make([]byte, 0, len(data)+32)
	if sc.comments {
		out = append(out, ": keepalive"...)
		out = append(out, sc.eol...)
	}
	if name != "" {
		out = append(out, "event: "...)
		out = append(out, name...)
		out = append(out, sc.eol...)
	}
	if sc.multiline {
		// Split at a separator between JSON members, where a newline is valid.
		if i := strings.Index(string(data), `,"`); i >= 0 {
			out = append(out, "data: "...)
			out = append(out, data[:i+1]...)
			out = append(out, sc.eol...)
			data = data[i+1:]
		}
	}
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, sc.eol...)
	return append(out, sc.eol...)
}
