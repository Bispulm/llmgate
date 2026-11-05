package sse

import "bytes"

// AppendData appends data to dst as one complete event. Any line ending within
// data starts a new data line, so a payload can never end the event early or
// inject another one.
func AppendData(dst, data []byte) []byte {
	for {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			break
		}
		dst = append(dst, "data: "...)
		dst = append(dst, data[:i]...)
		dst = append(dst, '\n')
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
	dst = append(dst, "data: "...)
	dst = append(dst, data...)
	return append(dst, '\n', '\n')
}
