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

// UnmarshalText parses a "<backend>/<model>" string, with an optional
// "?key=value" query carrying request-body overrides, into the Target. It
// implements [encoding.TextUnmarshaler] so a Target can be decoded directly
// from a TOML string.
func (t *Target) UnmarshalText(text []byte) error {
	ref, rawQuery, _ := strings.Cut(string(text), "?")
	backend, model, ok := strings.Cut(ref, "/")
	if !ok || backend == "" || model == "" {
		return fmt.Errorf("target %q must be <backend>/<model>", text)
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}
	t.backend = backend
	t.model = model
	t.query = query
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
