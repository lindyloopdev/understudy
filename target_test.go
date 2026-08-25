package understudy

import (
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gitlab.com/flimzy/testy/v2"
)

func TestTargetUnmarshalText(t *testing.T) {
	t.Parallel()

	type test struct {
		input        string
		wantIdentity string
		wantErr      string
	}

	tests := testy.NewTable[test]()

	tests.Add("should parse a backend/model string into its parts", test{
		input:        "opencode-go/deepseek-v4-flash",
		wantIdentity: "opencode-go/deepseek-v4-flash",
	})
	tests.Add("should reject a target with no slash", test{
		input:   "gpt-4",
		wantErr: `target "gpt-4" must be <backend>/<model>`,
	})
	tests.Add("should reject a target with an empty backend half", test{
		input:   "/deepseek-v4-flash",
		wantErr: `target "/deepseek-v4-flash" must be <backend>/<model>`,
	})
	tests.Add("should reject a target with an empty model half", test{
		input:   "opencode-go/",
		wantErr: `target "opencode-go/" must be <backend>/<model>`,
	})
	tests.Add("should decode a thinking query structurally without validating it", test{
		input:        "zai/glm-5?thinking=false",
		wantIdentity: "zai/glm-5",
	})
	tests.Add("should decode an override value without interpreting it, leaving that verdict to the caller", test{
		input:        "zai/glm-5?thinking=banana",
		wantIdentity: "zai/glm-5",
	})
	tests.Add("should decode a bare backend/model reference with no overrides", test{
		input:        "zai/glm-5",
		wantIdentity: "zai/glm-5",
	})
	tests.Add("should reject a malformed query string at decode", test{
		input:   "zai/glm-5?thinking=%zz",
		wantErr: "escape",
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		var got Target
		err := got.UnmarshalText([]byte(tt.input))
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error, got %v, want /%s/", err, tt.wantErr)
		}
		if err != nil {
			return
		}
		if got.identity() != tt.wantIdentity {
			t.Errorf("unexpected identity: got %q, want %q", got.identity(), tt.wantIdentity)
		}
	})
}

func TestTargetAccessors(t *testing.T) {
	t.Parallel()

	type test struct {
		input       string
		wantBackend string
		wantModel   string
		wantQuery   url.Values
	}

	tests := testy.NewTable[test]()

	tests.Add("should expose the backend, model, and query overrides the reference encoded", test{
		input:       "zai/glm-5?thinking=false",
		wantBackend: "zai",
		wantModel:   "glm-5",
		wantQuery:   url.Values{"thinking": {"false"}},
	})
	tests.Add("should expose a bare reference's backend and model with the empty query ParseTarget produced", test{
		input:       "opencode-go/deepseek-v4-flash",
		wantBackend: "opencode-go",
		wantModel:   "deepseek-v4-flash",
		wantQuery:   url.Values{},
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got, err := ParseTarget(tt.input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Backend() != tt.wantBackend {
			t.Errorf("unexpected backend: got %q, want %q", got.Backend(), tt.wantBackend)
		}
		if got.Model() != tt.wantModel {
			t.Errorf("unexpected model: got %q, want %q", got.Model(), tt.wantModel)
		}
		if d := cmp.Diff(tt.wantQuery, got.Query()); d != "" {
			t.Errorf("unexpected query: %s", d)
		}
	})
}

func TestTargetShouldKeepItsStoredQueryWhenACallerMutatesTheQueryItReturned(t *testing.T) {
	t.Parallel()

	got, err := ParseTarget("zai/glm-5?thinking=false")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := url.Values{"thinking": {"false"}}
	delete(got.Query(), "thinking")
	if d := cmp.Diff(want, got.Query()); d != "" {
		t.Errorf("unexpected query after mutating the returned values: %s", d)
	}
}

func TestTargetShouldKeepItsStoredQueryWhenACallerMutatesAnElementOfAValueSliceItReturned(t *testing.T) {
	t.Parallel()

	got, err := ParseTarget("zai/glm-5?thinking=false")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := url.Values{"thinking": {"false"}}
	returned := got.Query()
	returned["thinking"][0] = "mutated"
	if d := cmp.Diff(want, got.Query()); d != "" {
		t.Errorf("unexpected query after mutating the returned values: %s", d)
	}
}
