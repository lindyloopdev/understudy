package understudy

import (
	"bytes"
	"io"
)

// modelKey is the response field naming the model that answered.
var modelKey = []byte("model")

// maxModelNameBytes bounds the model name a response body may name and still
// be recorded: a longer value is not a model name worth holding per request.
const maxModelNameBytes = 256

// responseModelReader passes a relayed response body through to the client
// unchanged while reading the model a JSON object body names in its
// top-level "model" field. The name may differ from the model the attempt
// requested: a backend may answer under a name the requested one has since
// been aliased or replaced by. Names longer than maxModelNameBytes are not
// recorded. A body that is not a JSON object — a streamed SSE response, a
// bare error text — is settled by its first non-whitespace byte and never
// scanned further.
type responseModelReader struct {
	r     io.Reader
	state modelScanState
	// key holds the key being read; keyEsc marks one carrying an escape,
	// which rules a key out without unescaping it, since "model" has none.
	key    []byte
	keyEsc bool
	// modelKeyPending marks a top-level "model" key whose value is next.
	modelKeyPending bool
	// capture marks the string value being read as the model's, collected
	// into value.
	capture bool
	value   []byte
	// depth counts the containers open inside a non-string value being
	// skipped; inStr and escaped track a string within it, or the model
	// value.
	depth   int
	inStr   bool
	escaped bool
	// model is the model the body named, or "".
	model string
}

type modelScanState int

const (
	scanBodyStart modelScanState = iota
	scanMember
	scanKey
	scanColon
	scanValueStart
	scanString
	scanBareValue
	scanDone
)

func (m *responseModelReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	for i := range n {
		if m.state == scanDone {
			break
		}
		m.scan(p[i])
	}
	return n, err
}

// scan advances the read of one body byte.
func (m *responseModelReader) scan(b byte) {
	switch m.state {
	case scanBodyStart:
		switch {
		case b == '{':
			m.state = scanMember
		case isJSONSpace(b):
		default:
			m.state = scanDone
		}
	case scanMember:
		switch {
		case b == '"':
			m.key, m.keyEsc, m.escaped = m.key[:0], false, false
			m.state = scanKey
		case b == ',' || isJSONSpace(b):
		case b == '}':
			m.state = scanDone
		default:
			m.state = scanDone
		}
	case scanKey:
		switch {
		case m.escaped:
			m.escaped = false
		case b == '\\':
			m.escaped = true
			m.keyEsc = true
		case b == '"':
			m.modelKeyPending = !m.keyEsc && bytes.Equal(m.key, modelKey)
			m.state = scanColon
		case len(m.key) <= len(modelKey):
			// A prefix past "model" itself only ever rules the key out.
			m.key = append(m.key, b)
		}
	case scanColon:
		switch {
		case b == ':':
			m.state = scanValueStart
		case isJSONSpace(b):
		default:
			m.state = scanDone
		}
	case scanValueStart:
		switch {
		case b == '"':
			m.capture, m.escaped, m.value = m.modelKeyPending, false, m.value[:0]
			m.modelKeyPending = false
			m.state = scanString
		case isJSONSpace(b):
		default:
			m.modelKeyPending = false
			m.depth, m.inStr, m.escaped = 0, false, false
			m.state = scanBareValue
			m.scanBareValue(b)
		}
	case scanString:
		m.scanString(b)
	case scanBareValue:
		m.scanBareValue(b)
	}
}

// scanString reads a string value, collecting it while it is the model's.
func (m *responseModelReader) scanString(b byte) {
	switch {
	case m.escaped:
		m.escaped = false
	case b == '\\':
		m.escaped = true
		m.capture = false
	case b == '"':
		if m.capture {
			m.model = string(m.value)
			m.state = scanDone
			return
		}
		m.state = scanMember
	case !m.capture:
	case len(m.value) >= maxModelNameBytes:
		m.capture = false
	default:
		m.value = append(m.value, b)
	}
}

// scanBareValue skips a non-string value — a scalar or a nested container —
// until a separator or closing brace returns to the top level.
func (m *responseModelReader) scanBareValue(b byte) {
	if m.inStr {
		switch {
		case m.escaped:
			m.escaped = false
		case b == '\\':
			m.escaped = true
		case b == '"':
			m.inStr = false
		}
		return
	}
	switch b {
	case '"':
		m.inStr, m.escaped = true, false
	case '{', '[':
		m.depth++
	case '}', ']':
		if m.depth == 0 {
			m.state = scanDone
			return
		}
		m.depth--
	case ',':
		if m.depth == 0 {
			m.state = scanMember
		}
	}
}
