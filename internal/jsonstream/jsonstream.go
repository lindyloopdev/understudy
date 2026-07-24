// Package jsonstream provides streaming, byte-faithful rewrites of a single
// field within a JSON object or an SSE stream.
package jsonstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"slices"
)

// RewriteField rewrites the value of the top-level key within r's JSON object
// via the injected replace transform, streaming byte-faithfully: only the bytes
// up to and including the rewritten value are buffered; the remainder of r
// streams through untouched. replace receives the matched value's exact JSON
// literal (a scalar, or an entire nested object/array) and returns its
// replacement literal, which is spliced in place of the original. A body that
// is not a JSON object, or an object with no matching top-level key, passes
// through unchanged (replace is not called), leaving any missing-key policy to
// the caller.
//
// Any error — a read/parse failure before the value is spliced, or an error
// from replace itself — is reported alongside a non-nil reader that still
// relays r in full (the buffered prefix plus the unread remainder), so a caller
// that prefers to relay an unrewritable body rather than reject it can ignore
// the error and use the reader. The returned reader is never nil.
func RewriteField(r io.Reader, key string, replace func(raw []byte) ([]byte, error)) (io.Reader, error) {
	var prefixBuf bytes.Buffer
	tee := io.TeeReader(r, &prefixBuf)
	dec := json.NewDecoder(tee)

	firstTok, err := dec.Token()
	if err != nil {
		return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), err
	}
	if firstTok != json.Delim('{') {
		return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), nil
	}

	for {
		keyTok, err := dec.Token()
		if err != nil {
			return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), err
		}
		gotKey, ok := keyTok.(string)
		if !ok {
			return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), nil
		}
		if gotKey != key {
			// Skip this key's value whatever its shape: Decode consumes one
			// complete value (scalar or an entire nested object/array), so a
			// nested matching key is never mistaken for the top-level one.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), err
			}
			continue
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), err
		}
		// Decode leaves InputOffset immediately after the value's final byte
		// and raw holds the value with no surrounding whitespace, so the value
		// literal occupies prefix[afterValue-len(raw):afterValue]. Splice only
		// that span, keeping the ":" and any surrounding whitespace intact.
		afterValue := dec.InputOffset()
		valueStart := afterValue - int64(len(raw))

		newLiteral, err := replace(raw)
		if err != nil {
			return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), err
		}
		prefix := prefixBuf.Bytes()
		spliced := slices.Concat(prefix[:valueStart], newLiteral, prefix[afterValue:])
		return io.MultiReader(bytes.NewReader(spliced), r), nil
	}
}

const sseDataPrefix = "data: "

// RewriteFieldSSE is the SSE-framed counterpart of RewriteField: it rewrites
// the top-level key within each data: frame whose payload is a JSON object
// carrying it, and passes every other line — frames without the key, non-object
// payloads such as data: [DONE], non-data lines, and blank separators — through
// byte-for-byte.
func RewriteFieldSSE(r io.Reader, key string, replace func(raw []byte) ([]byte, error)) io.Reader {
	return &sseRewriter{br: bufio.NewReader(r), key: key, replace: replace}
}

type sseRewriter struct {
	br      *bufio.Reader
	key     string
	replace func(raw []byte) ([]byte, error)
	pending []byte
	err     error
}

func (s *sseRewriter) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		line, err := s.br.ReadBytes('\n')
		s.err = err
		if len(line) == 0 {
			return 0, s.err
		}
		s.pending = s.rewriteLine(line)
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	if len(s.pending) == 0 && s.err != nil {
		return n, s.err
	}
	return n, nil
}

func (s *sseRewriter) rewriteLine(line []byte) []byte {
	newline := []byte{}
	body := line
	if bytes.HasSuffix(body, []byte{'\n'}) {
		newline = []byte{'\n'}
		body = body[:len(body)-1]
	}
	if !bytes.HasPrefix(body, []byte(sseDataPrefix)) {
		return line
	}
	payload := body[len(sseDataPrefix):]
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	rewritten, err := RewriteField(bytes.NewReader(payload), s.key, s.replace)
	if err != nil {
		return line
	}
	out, _ := io.ReadAll(rewritten)
	return slices.Concat([]byte(sseDataPrefix), out, newline)
}
