package jsonstream_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gitlab.com/flimzy/testy/v2"

	"github.com/lindyloopdev/understudy/internal/jsonstream"
)

func TestRewriteField(t *testing.T) {
	t.Parallel()

	type test struct {
		reader   io.Reader
		key      string
		replace  func(raw []byte) ([]byte, error)
		wantBody string
		wantErr  string
		check    func()
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should splice a replacement for a nested object value while preserving the body byte-for-byte", func(t *testing.T) test {
		var gotRaw string
		return test{
			reader: strings.NewReader(`{"a":1,"usage":{"x":2,"y":3},"b":4}`),
			key:    "usage",
			replace: func(raw []byte) ([]byte, error) {
				gotRaw = string(raw)
				return []byte(`{"z":9}`), nil
			},
			wantBody: `{"a":1,"usage":{"z":9},"b":4}`,
			check: func() {
				if gotRaw != `{"x":2,"y":3}` {
					t.Errorf("replace got raw %q, want %q", gotRaw, `{"x":2,"y":3}`)
				}
			},
		}
	})

	tests.AddFunc("should splice a replacement for a nested array value while preserving the body byte-for-byte", func(t *testing.T) test {
		var gotRaw string
		return test{
			reader: strings.NewReader(`{"usage":[1,2,3],"b":4}`),
			key:    "usage",
			replace: func(raw []byte) ([]byte, error) {
				gotRaw = string(raw)
				return []byte(`[9]`), nil
			},
			wantBody: `{"usage":[9],"b":4}`,
			check: func() {
				if gotRaw != `[1,2,3]` {
					t.Errorf("replace got raw %q, want %q", gotRaw, `[1,2,3]`)
				}
			},
		}
	})

	passthrough := func(raw []byte) ([]byte, error) { return raw, nil }

	tests.Add("should relay the original body when the opening token cannot be parsed", test{
		reader:   strings.NewReader(`not-json`),
		key:      "usage",
		replace:  passthrough,
		wantErr:  `invalid character`,
		wantBody: `not-json`,
	})
	tests.Add("should relay the original body when a key token cannot be read", test{
		reader:   strings.NewReader(`{`),
		key:      "usage",
		replace:  passthrough,
		wantErr:  `EOF`,
		wantBody: `{`,
	})
	tests.Add("should relay the original body when a skipped value cannot be parsed", test{
		reader:   strings.NewReader(`{"other":`),
		key:      "usage",
		replace:  passthrough,
		wantErr:  `EOF`,
		wantBody: `{"other":`,
	})
	tests.Add("should relay the original body when the target value cannot be parsed", test{
		reader:   strings.NewReader(`{"usage":`),
		key:      "usage",
		replace:  passthrough,
		wantErr:  `EOF`,
		wantBody: `{"usage":`,
	})
	tests.Add("should relay the original body when replace fails", test{
		reader:   strings.NewReader(`{"usage":42}`),
		key:      "usage",
		replace:  func([]byte) ([]byte, error) { return nil, errors.New("boom") },
		wantErr:  `boom`,
		wantBody: `{"usage":42}`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		r, err := jsonstream.RewriteField(tt.reader, tt.key, tt.replace)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Fatalf("unexpected error: got %v, want /%s/", err, tt.wantErr)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if d := cmp.Diff(tt.wantBody, string(got)); d != "" {
			t.Errorf("body mismatch (-want +got):\n%s", d)
		}
		if tt.check != nil {
			tt.check()
		}
	})
}

func TestRewriteFieldSSE(t *testing.T) {
	t.Parallel()

	type test struct {
		input   string
		key     string
		replace func(raw []byte) ([]byte, error)
		want    string
	}

	tests := testy.NewTable[test]()

	tests.Add("should rewrite only the frame carrying the field", test{
		input: `data: {"choices":[{"delta":{"content":"Hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":100}}

data: [DONE]
`,
		key: "usage",
		replace: func([]byte) ([]byte, error) {
			return []byte(`{"rewritten":true}`), nil
		},
		want: `data: {"choices":[{"delta":{"content":"Hi"}}]}

data: {"choices":[],"usage":{"rewritten":true}}

data: [DONE]
`,
	})

	tests.Add("should pass an empty-payload data line through", test{
		input: "data: \n",
		key:   "usage",
		replace: func([]byte) ([]byte, error) {
			return nil, errors.New("replace must not be called for an empty payload")
		},
		want: "data: \n",
	})

	tests.Add("should pass an unrewritable frame through", test{
		input: `data: {"broken"

data: [DONE]
`,
		key: "usage",
		replace: func([]byte) ([]byte, error) {
			return []byte(`{"rewritten":true}`), nil
		},
		want: `data: {"broken"

data: [DONE]
`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		r := jsonstream.RewriteFieldSSE(strings.NewReader(tt.input), tt.key, tt.replace)

		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if d := cmp.Diff(tt.want, string(got)); d != "" {
			t.Errorf("stream mismatch (-want +got):\n%s", d)
		}
	})
}
