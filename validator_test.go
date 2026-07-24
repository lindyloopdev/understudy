package understudy

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gitlab.com/flimzy/testy/v2"

	"github.com/lindyloopdev/understudy/providers"
)

func TestConfigValidator(t *testing.T) {
	t.Parallel()

	type test struct {
		validator TokenValidator
		token     string
		want      *BackendConfig
		wantErr   string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return the configured backend when the static token matches", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		next := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			t.Error("next should not be called")
			return nil, nil
		}}
		cfg := Config{Token: "perm-token"}
		return test{
			validator: cfg.Validator(backend, next),
			token:     "perm-token",
			want:      backend,
		}
	}())

	tests.Add("should source the match token from c.Token", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		next := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			t.Error("next should not be called")
			return nil, nil
		}}
		cfg := Config{Token: "alpha"}
		return test{
			validator: cfg.Validator(backend, next),
			token:     "alpha",
			want:      backend,
		}
	}())

	tests.Add("should delegate to next when Token is empty", func() test {
		nextBackend := &BackendConfig{Backends: map[string]Backend{
			"openai": {ProviderType: "openai", Config: providers.Config{APIKey: "from-next"}},
		}}
		next := &stubValidator{ValidateFn: func(_ context.Context, token string) (*BackendConfig, error) {
			if token != "scene-token" {
				t.Errorf("next received token %q, want %q", token, "scene-token")
			}
			return nextBackend, nil
		}}
		cfg := Config{Token: ""}
		return test{
			validator: cfg.Validator(&BackendConfig{Backends: map[string]Backend{}}, next),
			token:     "scene-token",
			want:      nextBackend,
		}
	}())

	tests.Add("should delegate to next when Token is non-empty but the inbound token differs", func() test {
		nextBackend := &BackendConfig{Backends: map[string]Backend{
			"openai": {ProviderType: "openai", Config: providers.Config{APIKey: "from-next"}},
		}}
		next := &stubValidator{ValidateFn: func(_ context.Context, token string) (*BackendConfig, error) {
			if token != "scene-token" {
				t.Errorf("next received token %q, want %q", token, "scene-token")
			}
			return nextBackend, nil
		}}
		cfg := Config{Token: "perm-token"}
		return test{
			validator: cfg.Validator(&BackendConfig{Backends: map[string]Backend{}}, next),
			token:     "scene-token",
			want:      nextBackend,
		}
	}())

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got, err := tt.validator.Validate(t.Context(), tt.token)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error, got %s, want /%s/", err, tt.wantErr)
		}
		if d := cmp.Diff(tt.want, got); d != "" {
			t.Errorf("unexpected result: %s", d)
		}
	})
}

func TestSingleToken(t *testing.T) {
	t.Parallel()

	type test struct {
		validator TokenValidator
		token     string
		want      *BackendConfig
		wantErr   string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return the configured backend when the token matches", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		return test{
			validator: SingleToken("perm-token", backend),
			token:     "perm-token",
			want:      backend,
		}
	}())

	tests.Add("should reject a non-matching token with ErrInvalidToken", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		return test{
			validator: SingleToken("perm-token", backend),
			token:     "wrong-token",
			want:      nil,
			wantErr:   "invalid token",
		}
	}())

	tests.Add("should reject an empty inbound token when the configured token is empty", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		return test{
			validator: SingleToken("", backend),
			token:     "",
			want:      nil,
			wantErr:   "invalid token",
		}
	}())

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got, err := tt.validator.Validate(t.Context(), tt.token)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error, got %s, want /%s/", err, tt.wantErr)
		}
		if d := cmp.Diff(tt.want, got); d != "" {
			t.Errorf("unexpected result: %s", d)
		}
	})
}

func TestStaticToken(t *testing.T) {
	t.Parallel()

	type test struct {
		validator TokenValidator
		token     string
		want      *BackendConfig
		wantErr   string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return the configured backend without consulting next when the token matches", func() test {
		backend := &BackendConfig{Backends: map[string]Backend{}}
		next := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			t.Error("next should not be called")
			return nil, nil
		}}
		return test{
			validator: StaticToken("perm-token", backend, next),
			token:     "perm-token",
			want:      backend,
		}
	}())

	tests.Add("should delegate to next with the inbound token when the token does not match", func() test {
		nextBackend := &BackendConfig{Backends: map[string]Backend{
			"openai": {ProviderType: "openai", Config: providers.Config{APIKey: "from-next"}},
		}}
		next := &stubValidator{ValidateFn: func(_ context.Context, token string) (*BackendConfig, error) {
			if token != "wrong-token" {
				t.Errorf("next received token %q, want %q", token, "wrong-token")
			}
			return nextBackend, nil
		}}
		return test{
			validator: StaticToken("perm-token", &BackendConfig{Backends: map[string]Backend{}}, next),
			token:     "wrong-token",
			want:      nextBackend,
		}
	}())

	tests.Add("should delegate an empty inbound token to next when no static token is configured", func() test {
		nextBackend := &BackendConfig{Backends: map[string]Backend{
			"openai": {ProviderType: "openai", Config: providers.Config{APIKey: "from-next"}},
		}}
		next := &stubValidator{ValidateFn: func(_ context.Context, token string) (*BackendConfig, error) {
			if token != "" {
				t.Errorf("next received token %q, want empty", token)
			}
			return nextBackend, nil
		}}
		return test{
			validator: StaticToken("", &BackendConfig{Backends: map[string]Backend{}}, next),
			token:     "",
			want:      nextBackend,
		}
	}())

	tests.Add("should propagate next's error with a nil backend when the token does not match", func() test {
		next := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return nil, ErrInvalidToken
		}}
		return test{
			validator: StaticToken("perm-token", &BackendConfig{Backends: map[string]Backend{}}, next),
			token:     "wrong-token",
			wantErr:   "invalid token",
		}
	}())

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got, err := tt.validator.Validate(t.Context(), tt.token)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error, got %s, want /%s/", err, tt.wantErr)
		}
		if d := cmp.Diff(tt.want, got); d != "" {
			t.Errorf("unexpected result: %s", d)
		}
	})
}
