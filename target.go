package understudy

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// thinkingParam is the query-parameter key for the thinking override.
const thinkingParam = "thinking"

// Target is a concrete <backend>/<model> model target with optional per-target
// request-body overrides carried in a "?key=value" query.
//
// TODO(TODO.d/understudy-thinking-disable.md): enabling thinking (thinking=true)
// is reserved and unimplemented — a future tri-state override.
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

// validate reports whether the target's query overrides are acceptable. The
// "thinking" override must be a strict boolean; thinking=true is reserved.
// Unknown keys are ignored.
func (t Target) validate() error {
	if t.query.Has(thinkingParam) {
		raw := t.query.Get(thinkingParam)
		thinking, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid thinking value %q: %w", raw, err)
		}
		if thinking {
			return errors.New("thinking=true is reserved: enabling thinking is not yet supported")
		}
	}
	return nil
}

// identity returns the "<backend>/<model>" string identifying the target's
// upstream, independent of any per-target overrides. It is the availability key,
// so different override profiles of one model share health.
func (t Target) identity() string { return t.backend + "/" + t.model }

// disablesThinking reports whether the target's "thinking" override disables
// thinking (present and parsing to false).
func (t Target) disablesThinking() bool {
	disabled, err := strconv.ParseBool(t.query.Get(thinkingParam))
	return err == nil && !disabled
}
