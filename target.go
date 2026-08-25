package understudy

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// reservedOverrideKeys are the query keys a target reference may not carry.
// They define what the request is (model, messages, tools) or the client's own
// contract for how the response arrives and is shaped (stream, response_format,
// n, tool_choice, parallel_tool_calls) — not a generation-behavior tweak — so
// an override would speak for the caller rather than adjust generation.
var reservedOverrideKeys = []string{
	"model",
	"messages",
	"stream",
	"tools",
	"response_format",
	"n",
	"tool_choice",
	"parallel_tool_calls",
}

// Target is a concrete <backend>/<model> model target with optional per-target
// request-body overrides carried in a "?key=value" query: each non-reserved key
// is forwarded in the request body under that key, set to its value's JSON
// interpretation.
//
// Overrides are unsafe in Go's sense: only the key is checked against
// reservedOverrideKeys, never its value's shape. ?thinking=true forwards the
// literal boolean true, not the object a backend may expect — the caller's to
// get right. See DESIGN.md §Understudy "Per-target request-body normalization".
type Target struct {
	backend string
	model   string
	query   url.Values
}

// malformedRefFormat is one wording for both shape malformations — no separator,
// or an empty half — because a config document cannot act on the difference. An
// unparseable query surfaces the URL parser's own error instead.
const malformedRefFormat = "target %q must be <backend>/<model>"

// notAReferenceError marks a separator-less string: a bare model name, not a
// malformed reference. Callers that route the two differently match on it.
type notAReferenceError struct{ ref string }

func (e notAReferenceError) Error() string { return fmt.Sprintf(malformedRefFormat, e.ref) }

// ParseTarget parses a "<backend>/<model>" reference, with an optional
// "?key=value" query carrying request-body overrides. It is the only way to
// build a Target: the same reference means the same thing whether an operator
// wrote it in a config or a caller named it as a request's model.
func ParseTarget(s string) (Target, error) {
	ref, rawQuery, _ := strings.Cut(s, "?")
	backend, model, ok := strings.Cut(ref, "/")
	if !ok {
		return Target{}, notAReferenceError{ref: s}
	}
	if backend == "" || model == "" {
		return Target{}, fmt.Errorf(malformedRefFormat, s)
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return Target{}, err
	}
	return Target{backend: backend, model: model, query: query}, nil
}

// UnmarshalText implements [encoding.TextUnmarshaler]. It settles only the
// grammar: a decoded Target's overrides are unchecked, so [Target.validate] still
// has to run — config load and the request boundary each do that themselves.
func (t *Target) UnmarshalText(text []byte) error {
	parsed, err := ParseTarget(string(text))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// MarshalText renders the target back into its "<backend>/<model>" string,
// with any request-body overrides as a "?key=value" query. It implements
// [encoding.TextMarshaler] so a Target survives the TOML round-trip that
// daemon registration performs.
func (t Target) MarshalText() ([]byte, error) {
	s := t.identity()
	if overrides := t.query.Encode(); overrides != "" {
		s += "?" + overrides
	}
	return []byte(s), nil
}

// validate reports whether the target's query overrides are acceptable. Any
// key is a valid override except the reserved ones, which would rewrite what
// the request is or the caller's response contract rather than tweak
// generation behavior. A repeated key is also rejected: it can only carry one
// value into the body, so a second occurrence can never mean anything a single
// query built from one source would — it is a duplication mistake, not two
// values to reconcile.
func (t Target) validate() error {
	for _, key := range slices.Sorted(maps.Keys(t.query)) {
		if slices.Contains(reservedOverrideKeys, key) {
			return fmt.Errorf("override key %q is reserved", key)
		}
		if len(t.query[key]) > 1 {
			return fmt.Errorf("override key %q is repeated", key)
		}
	}
	return nil
}

// Backend returns the backend half of the parsed "<backend>/<model>"
// reference.
func (t Target) Backend() string { return t.backend }

// Model returns the model half of the parsed "<backend>/<model>" reference.
func (t Target) Model() string { return t.model }

// Query returns the reference's query, carrying any per-target request-body
// overrides. It is empty, never nil, for a bare reference.
func (t Target) Query() url.Values {
	out := make(url.Values, len(t.query))
	for k, v := range t.query {
		out[k] = slices.Clone(v)
	}
	return out
}

// identity returns the "<backend>/<model>" string identifying the target's
// upstream, independent of any per-target overrides. It is the availability key,
// so different override profiles of one model share health.
func (t Target) identity() string { return t.backend + "/" + t.model }
