// Package jsonframe implements the Content-Length message framing shared by
// the Language Server Protocol and the Debug Adapter Protocol.
//
// Both wrap JSON bodies in an HTTP-style header block:
//
//	Content-Length: 42\r\n
//	\r\n
//	{"jsonrpc":"2.0",...}
//
// It is a small format, but getting it subtly wrong produces hangs rather than
// errors, so it lives in one place with one set of tests instead of once per
// protocol client.
package jsonframe

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxMessageBytes bounds a single message. A malformed or hostile
// Content-Length would otherwise allocate whatever it asked for.
const maxMessageBytes = 32 << 20 // 32 MiB

// Read returns the next message body. Headers other than Content-Length are
// ignored, and header names are matched case-insensitively, because both
// protocols permit extra headers and neither guarantees casing.
func Read(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line ends the header block
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("jsonframe: bad Content-Length %q", value)
		}
		length = n
	}
	switch {
	case length < 0:
		return nil, fmt.Errorf("jsonframe: message has no Content-Length header")
	case length == 0:
		return []byte{}, nil
	case length > maxMessageBytes:
		return nil, fmt.Errorf("jsonframe: message of %d bytes exceeds the %d byte limit", length, maxMessageBytes)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// Write frames and sends one message body.
func Write(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}
