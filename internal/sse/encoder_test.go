package sse

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestAppendDataFormatsAnEvent(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"single line", `{"a":1}`, "data: {\"a\":1}\n\n"},
		{"newlines become data lines", "a\nb", "data: a\ndata: b\n\n"},
		{"CRLF and CR cannot end the event early", "a\r\nb\rc", "data: a\ndata: b\ndata: c\n\n"},
		{"empty", "", "data: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AppendData(nil, []byte(tt.input)); string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendDataAppendsToExistingBytes(t *testing.T) {
	got := AppendData([]byte("x"), []byte("y"))
	if string(got) != "xdata: y\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestAppendDataRoundTripsThroughTheDecoder(t *testing.T) {
	for _, input := range []string{"plain", "two\nlines", " leading space", "", "héllo 世界"} {
		d := NewDecoder(bytes.NewReader(AppendData(nil, []byte(input))), testLimit)
		ev, err := d.Next()
		if err != nil {
			t.Fatalf("input %q: %v", input, err)
		}
		if string(ev.Data) != input {
			t.Fatalf("data = %q, want %q", ev.Data, input)
		}
		if _, err := d.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("input %q: trailing error %v", input, err)
		}
	}
}
