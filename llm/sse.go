package llm

import (
	"bufio"
	"io"
	"strings"
)

// SSEScanner reads Server-Sent Events from an io.Reader, yielding just the
// data payload of each event (without the "data: " prefix).
//
// It is compliant with the SSE specification regarding multi-line events:
// multiple consecutive `data:` lines within a single event are joined with
// U+000A LINE FEED characters, and a single trailing line feed is stripped
// before dispatch (so `data: a` → "a" and `data: a` + `data: b` → "a\nb").
// Events are dispatched on a blank line (the event separator) or at EOF.
//
// The OpenAI `[DONE]` sentinel is delivered unchanged as a sole-line event so
// callers can detect stream termination.
type SSEScanner struct {
	reader *bufio.Reader
	line   string
	err    error
}

func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{reader: bufio.NewReader(r)}
}

func (s *SSEScanner) Scan() bool {
	var dataBuf strings.Builder
	hasData := false

	for {
		raw, err := s.reader.ReadString('\n')
		isEOF := err == io.EOF
		if err != nil && !isEOF {
			s.err = err
			return false
		}

		line := strings.TrimRight(raw, "\r\n")

		// Blank line = event separator; comment lines start with ':'.
		// Either way, if we have accumulated data, dispatch it now.
		if line == "" || strings.HasPrefix(line, ":") {
			if hasData {
				s.line = strings.TrimSuffix(dataBuf.String(), "\n")
				return true
			}
			if isEOF {
				return false
			}
			continue
		}

		// data: field — accumulate (spec: append value + LF, strip trailing LF on dispatch).
		switch {
		case strings.HasPrefix(line, "data: "):
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
			dataBuf.WriteByte('\n')
			hasData = true
		case line == "data:":
			dataBuf.WriteByte('\n')
			hasData = true
		default:
			// event:, id:, retry: and any other fields are part of the event
			// but carry no payload we expose; keep accumulating until separator.
		}

		if isEOF {
			// Stream ended without a trailing blank line; flush pending event.
			if hasData {
				s.line = strings.TrimSuffix(dataBuf.String(), "\n")
				return true
			}
			return false
		}
	}
}

func (s *SSEScanner) Text() string {
	return s.line
}

func (s *SSEScanner) Err() error {
	return s.err
}
