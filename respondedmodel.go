package understudy

import (
	"bytes"
	"encoding/json/jsontext"
)

// maxBodyPrefixBytes bounds how much of a relayed response body is kept to
// read the model it names from: a model named within the first
// maxBodyPrefixBytes is found, wherever it falls in them, and one named only
// after them is recorded as naming none.
const maxBodyPrefixBytes = 64 << 10

// bodyPrefix keeps the first maxBodyPrefixBytes written to it and drops the
// rest, so a relayed body can be teed to it without being held in full.
type bodyPrefix struct {
	b []byte
}

func (p *bodyPrefix) Write(b []byte) (int, error) {
	if room := maxBodyPrefixBytes - len(p.b); room > 0 {
		p.b = append(p.b, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

// modelNamedBy returns the model a response body names in its top-level
// "model" field, or "". The name may differ from the model the attempt
// requested: a backend may answer under a name the requested one has since
// been aliased or replaced by. A JSON object body names it directly; a
// server-sent-events body names it in a chunk whose "data:" payload is a JSON
// object with that field, and the first such chunk to name one settles it. Any
// other body names none. body may be truncated; only what it holds is read.
func modelNamedBy(body []byte) string {
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); bytes.HasPrefix(trimmed, []byte("{")) {
		return modelInObject(trimmed)
	}
	for line := range bytes.Lines(body) {
		payload, ok := bytes.CutPrefix(line, dataField)
		if !ok {
			continue
		}
		payload = bytes.TrimLeft(payload, " \t")
		if !bytes.HasPrefix(payload, []byte("{")) {
			continue
		}
		if model := modelInObject(payload); model != "" {
			return model
		}
	}
	return ""
}

// modelInObject returns the model a JSON object names in its top-level
// "model" field, or "" when it names none, names a value that is not a plain
// string, or ends before the field. A value carrying an escape is returned
// as the string it decodes to.
func modelInObject(obj []byte) string {
	dec := jsontext.NewDecoder(bytes.NewReader(obj), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if _, err := dec.ReadToken(); err != nil {
		return ""
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return ""
		}
		if name.String() != "model" || dec.PeekKind() != '"' {
			if dec.SkipValue() != nil {
				return ""
			}
			continue
		}
		value, err := dec.ReadToken()
		if err != nil {
			return ""
		}
		return value.String()
	}
	return ""
}
