package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	Name string
	Data string
}

// sseScanner reads server-sent events from a response body.
type sseScanner struct {
	reader *bufio.Reader
	event  sseEvent
	err    error
}

// newSSEScanner wraps a stream. The buffer is generous because a single Codex or
// Gemini event can carry a large JSON payload.
func newSSEScanner(r io.Reader) *sseScanner {
	reader := bufio.NewReaderSize(r, 64*1024)
	return &sseScanner{reader: reader}
}

// Scan advances to the next event, returning false at end of stream.
func (s *sseScanner) Scan() bool {
	if s.err != nil {
		return false
	}

	var (
		name string
		data strings.Builder
	)
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && data.Len() > 0 {
				s.event = sseEvent{Name: name, Data: data.String()}
				s.err = io.EOF
				return true
			}
			s.err = err
			return false
		}

		switch {
		case line == "":
			// Blank line terminates the event.
			if data.Len() == 0 && name == "" {
				continue
			}
			s.event = sseEvent{Name: name, Data: data.String()}
			return true
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive.
			continue
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(chunk)
		default:
			// id: / retry: / unknown field, ignored.
		}
	}
}

// readLine reads one line, transparently handling long lines and CRLF.
func (s *sseScanner) readLine() (string, error) {
	var b strings.Builder
	for {
		chunk, isPrefix, err := s.reader.ReadLine()
		if err != nil {
			if b.Len() > 0 && err == io.EOF {
				return b.String(), nil
			}
			return "", err
		}
		b.Write(chunk)
		if !isPrefix {
			return b.String(), nil
		}
	}
}

// Event returns the most recent event.
func (s *sseScanner) Event() sseEvent { return s.event }

// Err returns the terminating error, excluding a clean end of stream.
func (s *sseScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

// sseWriter emits server-sent events to a client, flushing after each one.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	err     error
}

// newSSEWriter prepares w for streaming and writes the SSE headers.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// Stops nginx and friends from buffering the stream.
	header.Set("X-Accel-Buffering", "no")

	sw := &sseWriter{w: w}
	if flusher, ok := w.(http.Flusher); ok {
		sw.flusher = flusher
	}
	return sw
}

// WriteHeader sends the status line and flushes it so the client sees the stream
// start immediately.
func (s *sseWriter) WriteHeader(status int) {
	s.w.WriteHeader(status)
	s.Flush()
}

// Data writes an unnamed event.
func (s *sseWriter) Data(payload string) {
	s.Event("", payload)
}

// Event writes a named event.
func (s *sseWriter) Event(name, payload string) {
	if s.err != nil {
		return
	}
	var b strings.Builder
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	// Multi-line payloads need a data: prefix per line.
	for _, line := range strings.Split(payload, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if _, err := io.WriteString(s.w, b.String()); err != nil {
		s.err = err
		return
	}
	s.Flush()
}

// Comment writes a keep-alive comment.
func (s *sseWriter) Comment(text string) {
	if s.err != nil {
		return
	}
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		s.err = err
		return
	}
	s.Flush()
}

// Flush pushes buffered bytes to the client.
func (s *sseWriter) Flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Err reports the first write error, which means the client hung up.
func (s *sseWriter) Err() error { return s.err }
