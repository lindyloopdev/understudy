package understudy

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/go-playground/validator/v10"
	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gitlab.com/flimzy/testy/v2"

	"github.com/lindyloopdev/understudy/providers"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("mustParseURL(%q): %v", raw, err)
	}
	return u
}

func TestConfigShouldSurviveTheTOMLRoundTripDaemonRegistrationPerforms(t *testing.T) {
	t.Parallel()

	type test struct {
		doc string
	}

	tests := testy.NewTable[test]()

	tests.Add("should round-trip model targets, overrides included", test{
		doc: `
[backends.opencode-go]
provider_type = "openai"
base_url = "https://opencode.ai/zen/go/v1"
api_key = "sk-test"

[models.review-light]
targets = ["opencode-go/deepseek-v4-flash", "opencode-go/glm-4.7?thinking=false"]
`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		var registered Config
		if err := toml.Unmarshal([]byte(tt.doc), &registered); err != nil {
			t.Fatalf("unmarshaling the source config: %v", err)
		}

		wire, err := toml.Marshal(registered)
		if err != nil {
			t.Fatalf("marshaling the registration payload: %v", err)
		}

		var received Config
		if err := toml.Unmarshal(wire, &received); err != nil {
			t.Fatalf("daemon-side unmarshal rejected the payload: %v\npayload was:\n%s", err, wire)
		}

		if d := gocmp.Diff(registered, received, gocmp.AllowUnexported(Target{})); d != "" {
			t.Errorf("config differs after the registration round-trip (-registered +received):\n%s", d)
		}
	})
}

func TestConfigResolve(t *testing.T) {
	t.Parallel()

	type test struct {
		cfg     Config
		want    *BackendConfig
		wantErr string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return parsed backend config for a valid openai backend", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"groq": {ProviderType: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-test"},
			},
		},
		want: &BackendConfig{
			Backends: map[string]Backend{
				"groq": {ProviderType: "openai", Config: providers.Config{
					BaseURL: mustParseURL(t, "https://api.openai.com"),
					APIKey:  "sk-test",
				}},
			},
		},
	})

	tests.AddFunc("should read the backend API key from api_key_file trimmed of surrounding whitespace", func(t *testing.T) test {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("sk-fromfile\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return test{
			cfg: Config{
				Backends: map[string]BackendSpec{
					"groq": {ProviderType: "openai", BaseURL: "https://api.openai.com", APIKeyFile: path},
				},
			},
			want: &BackendConfig{
				Backends: map[string]Backend{
					"groq": {ProviderType: "openai", Config: providers.Config{
						BaseURL: mustParseURL(t, "https://api.openai.com"),
						APIKey:  "sk-fromfile",
					}},
				},
			},
		}
	})

	tests.Add("should return error when a backend api_key_file is unreadable", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"groq": {ProviderType: "openai", BaseURL: "https://api.openai.com", APIKeyFile: "/nonexistent/lindy-key"},
			},
		},
		wantErr: `understudy\.backends\.groq: read api_key_file "/nonexistent/lindy-key": .+`,
	})

	tests.AddFunc("should return error when a backend api_key_file is empty", func(t *testing.T) test {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("  \n\t "), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return test{
			cfg: Config{
				Backends: map[string]BackendSpec{
					"groq": {ProviderType: "openai", BaseURL: "https://api.openai.com", APIKeyFile: path},
				},
			},
			wantErr: `understudy\.backends\.groq: api_key_file ".+" is empty`,
		}
	})

	tests.Add("should return error when a backend api_key_file is relative", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"groq": {ProviderType: "openai", BaseURL: "https://api.openai.com", APIKeyFile: "relative/lindy-key"},
			},
		},
		wantErr: `understudy\.backends\.groq: api_key_file "relative/lindy-key" must be an absolute path`,
	})

	tests.Add("should return error when a backend base_url is invalid", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"groq": {ProviderType: "openai", BaseURL: "://bad"},
			},
		},
		wantErr: `understudy\.backends\.groq: invalid base_url "://bad"`,
	})

	tests.Add("should return an empty BackendConfig when Backends is nil", test{
		cfg:  Config{},
		want: &BackendConfig{Backends: map[string]Backend{}},
	})

	tests.Add("should resolve a logical model's targets into BackendConfig.Models", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
				"b": {ProviderType: "openai", BaseURL: "https://b.example.com", APIKey: "sk-b"},
			},
			Models: map[string]LogicalModelSpec{
				"cheap": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}},
			},
		},
		want: &BackendConfig{
			Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{
					BaseURL: mustParseURL(t, "https://a.example.com"),
					APIKey:  "sk-a",
				}},
				"b": {ProviderType: "openai", Config: providers.Config{
					BaseURL: mustParseURL(t, "https://b.example.com"),
					APIKey:  "sk-b",
				}},
			},
			Models: map[string]LogicalModel{
				"cheap": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}},
			},
		},
	})

	tests.Add("should accept a thinking=false target at resolve", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
			},
			Models: map[string]LogicalModelSpec{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"thinking": {"false"}}}}},
			},
		},
		want: &BackendConfig{
			Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{
					BaseURL: mustParseURL(t, "https://a.example.com"),
					APIKey:  "sk-a",
				}},
			},
			Models: map[string]LogicalModel{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"thinking": {"false"}}}}},
			},
		},
	})

	tests.Add("should reject a non-boolean thinking value at resolve", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
			},
			Models: map[string]LogicalModelSpec{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"thinking": {"maybe"}}}}},
			},
		},
		wantErr: `understudy\.models\.m: target "a/ma": invalid thinking value`,
	})

	tests.Add("should reject a reserved thinking=true value at resolve", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
			},
			Models: map[string]LogicalModelSpec{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"thinking": {"true"}}}}},
			},
		},
		wantErr: "reserved",
	})

	tests.Add("should ignore an unknown query parameter at resolve", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
			},
			Models: map[string]LogicalModelSpec{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"foo": {"bar"}}}}},
			},
		},
		want: &BackendConfig{
			Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{
					BaseURL: mustParseURL(t, "https://a.example.com"),
					APIKey:  "sk-a",
				}},
			},
			Models: map[string]LogicalModel{
				"m": {Targets: []Target{{backend: "a", model: "ma", query: url.Values{"foo": {"bar"}}}}},
			},
		},
	})

	tests.Add("should reject a logical model target on an unknown backend", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"a": {ProviderType: "openai", BaseURL: "https://a.example.com", APIKey: "sk-a"},
			},
			Models: map[string]LogicalModelSpec{
				"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "ghost", model: "mb"}}},
			},
		},
		wantErr: `understudy\.models\.m: target "ghost/mb" references unknown backend "ghost"`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got, err := tt.cfg.Resolve()
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error, got %v, want /%s/", err, tt.wantErr)
		}
		if d := gocmp.Diff(tt.want, got, cmpopts.IgnoreFields(providers.Config{}, "HTTPClient"), gocmp.AllowUnexported(Target{})); d != "" {
			t.Errorf("Resolve() mismatch (-want +got):\n%s", d)
		}
	})
}

func TestConfigDefaultModel(t *testing.T) {
	t.Parallel()

	type test struct {
		cfg  Config
		want string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return the default logical model when a backend is configured", test{
		cfg: Config{
			Backends: map[string]BackendSpec{
				"primary": {
					ProviderType: "openai",
					BaseURL:      "https://api.openai.com/v1",
					APIKey:       "sk-test",
				},
			},
		},
		want: DefaultLogicalModel,
	})

	tests.Add("should return no model when no backend is configured", test{
		cfg:  Config{},
		want: "",
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got := tt.cfg.DefaultModel()
		if d := gocmp.Diff(tt.want, got); d != "" {
			t.Errorf("DefaultModel() mismatch (-want +got):\n%s", d)
		}
	})
}

// TestRegisteredProvidersPassValidation guards the SSOT coupling: every
// provider understudy registers by default must be accepted by the
// provider_type oneof constraint on BackendSpec. Registering a provider
// without extending the oneof would silently leave config validation
// rejecting it.
func TestRegisteredProvidersPassValidation(t *testing.T) {
	t.Parallel()

	validate := validator.New()
	for provider := range defaultProviders() {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			spec := BackendSpec{
				ProviderType: provider,
				BaseURL:      "https://example.com",
				APIKey:       "sk-test",
			}
			if err := validate.Struct(spec); err != nil {
				t.Errorf("registered provider %q rejected by validation: %v", provider, err)
			}
		})
	}
}

func TestBackendSpecKeySourceValidation(t *testing.T) {
	t.Parallel()

	type test struct {
		spec  BackendSpec
		valid bool
	}

	tests := testy.NewTable[test]()

	tests.Add("should accept a backend whose key comes only from api_key_file", test{
		spec:  BackendSpec{ProviderType: "openai", BaseURL: "https://example.com", APIKeyFile: "/some/path"},
		valid: true,
	})

	tests.Add("should accept a backend whose key comes only from api_key", test{
		spec:  BackendSpec{ProviderType: "openai", BaseURL: "https://example.com", APIKey: "sk-test"},
		valid: true,
	})

	tests.Add("should reject a backend that supplies neither api_key nor api_key_file", test{
		spec:  BackendSpec{ProviderType: "openai", BaseURL: "https://example.com"},
		valid: false,
	})

	tests.Add("should reject a backend that supplies both api_key and api_key_file", test{
		spec:  BackendSpec{ProviderType: "openai", BaseURL: "https://example.com", APIKey: "sk-test", APIKeyFile: "/some/path"},
		valid: false,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		err := validator.New().Struct(tt.spec)
		if (err == nil) != tt.valid {
			t.Errorf("validation valid=%v, want %v (err: %v)", err == nil, tt.valid, err)
		}
	})
}
