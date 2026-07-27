package jsonframe

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func frame(body string) string {
	return "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

func TestReadParsesFraming(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	got, err := Read(bufio.NewReader(strings.NewReader(frame(body))))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

// Both protocols allow extra headers and neither guarantees casing.
func TestReadToleratesHeaderVariation(t *testing.T) {
	body := `{"a":1}`
	raw := "content-length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n" + body
	got, err := Read(bufio.NewReader(strings.NewReader(raw)))
	if err != nil || string(got) != body {
		t.Errorf("got %q, %v", got, err)
	}
}

// Several messages arrive on one stream; each read must consume exactly one.
func TestReadConsumesOneMessageAtATime(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(frame(`{"n":1}`) + frame(`{"n":2}`)))
	for _, want := range []string{`{"n":1}`, `{"n":2}`} {
		got, err := Read(r)
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
}

// A missing or malformed length must error rather than block forever.
func TestReadRejectsBadHeaders(t *testing.T) {
	cases := map[string]string{
		"no length":     "X-Other: 1\r\n\r\n{}",
		"unparseable":   "Content-Length: abc\r\n\r\n{}",
		"absurd length": "Content-Length: 999999999999\r\n\r\n{}",
	}
	for name, raw := range cases {
		if _, err := Read(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestReadZeroLengthBody(t *testing.T) {
	got, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 0\r\n\r\n")))
	if err != nil || len(got) != 0 {
		t.Errorf("got %q, %v; want an empty body", got, err)
	}
}

// A truncated body is a broken stream, not an empty message.
func TestReadTruncatedBody(t *testing.T) {
	if _, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 100\r\n\r\nshort"))); err == nil {
		t.Error("expected an error for a body shorter than its declared length")
	}
}

func TestWriteRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	body := []byte(`{"command":"initialize"}`)
	if err := Write(&buf, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "Content-Length: 24\r\n\r\n") {
		t.Errorf("unexpected framing: %q", buf.String())
	}
	got, err := Read(bufio.NewReader(&buf))
	if err != nil || string(got) != string(body) {
		t.Errorf("round trip gave %q, %v", got, err)
	}
}
