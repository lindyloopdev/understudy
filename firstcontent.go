package understudy

import (
	"bytes"
	"io"
)

// confirmingReader watches a relayed response body and calls content at its
// first content: the first `data:` line of an SSE stream, or the first
// non-whitespace byte of any other body. The body's bytes, not the response's
// Content-Type, pick the rule; a stream is any opening SSE field line or ':'
// comment, and only `data:` lines carry response content. After confirmation
// the body passes through unscanned and unretained; before it, only a bounded
// prefix of the in-progress line is kept, since a pre-content line's length
// is unbounded.
type confirmingReader struct {
	r         io.Reader
	stream    bool
	content   func()
	line      []byte
	confirmed bool
}

func (c *confirmingReader) Read(buf []byte) (int, error) {
	n, err := c.r.Read(buf)
	if n > 0 && !c.confirmed {
		c.scan(buf[:n])
	}
	return n, err
}

// sseFieldNames are SSE's four field names, colon included. The list is
// closed rather than any `name:` prefix: reading a custom field line as
// content fires only early, but reading a plain-text body (`Error: upstream
// unavailable`) as a stream waits for a `data:` line that never arrives, and
// the next queued request is never admitted.
var sseFieldNames = []string{"data:", "event:", "id:", "retry:"}

// dataField opens the only SSE line that carries response content.
var dataField = []byte("data:")

// sseFieldPrefix reports whether buf is a prefix of, or a whole, SSE field
// name.
func sseFieldPrefix(buf []byte) (prefix, complete bool) {
	for _, name := range sseFieldNames {
		if bytes.HasPrefix([]byte(name), buf) {
			if len(buf) == len(name) {
				return true, true
			}
			prefix = true
		}
	}
	return prefix, false
}

// scan scans buf for the first content, carrying the in-progress stream line
// across reads.
func (c *confirmingReader) scan(buf []byte) {
	for _, b := range buf {
		if c.stream {
			if b == '\n' {
				if bytes.HasPrefix(c.line, dataField) {
					c.firstContent()
					return
				}
				c.line = c.line[:0]
				continue
			}
			// Only enough of the line to recognize dataField at its newline is
			// kept; the rest is dropped.
			if len(c.line) < len(dataField) {
				c.line = append(c.line, b)
			}
			continue
		}
		if len(c.line) == 0 && isJSONSpace(b) {
			continue
		}
		c.line = append(c.line, b)
		if c.line[0] == ':' {
			// SSE comment line.
			c.stream = true
			continue
		}
		partial, field := sseFieldPrefix(c.line)
		switch {
		case field:
			c.stream = true
		case partial:
			// c.line is bounded: a field name is at most `retry:`.
		default:
			c.firstContent()
			return
		}
	}
}

// firstContent calls content once and drops any line still being built.
func (c *confirmingReader) firstContent() {
	if !c.confirmed {
		c.confirmed = true
		c.line = nil
		c.content()
	}
}
