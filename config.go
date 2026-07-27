package understudy

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/lindyloopdev/understudy/providers"
)

// Config is the parsed `[understudy]` configuration section: an optional
// permanent token, the uniquely-named backends it resolves to, and any logical
// models (named ordered target lists for failover). Embedders bind it from
// their own config file and call [Config.Resolve].
type Config struct {
	Token    string                 `toml:"token"`
	Backends map[string]BackendSpec `toml:"backends" validate:"dive"`

	// Models maps a logical model name to its spec, sourced from the
	// `[understudy.models.<name>]` tables.
	Models map[string]LogicalModelSpec `toml:"models"`
}

// LogicalModelSpec is a single `[understudy.models.<name>]` table. The table
// key is the logical model name; Targets is its ordered list of concrete
// failover targets.
type LogicalModelSpec struct {
	Targets []Target `toml:"targets"`
}

// BackendSpec is a single `[understudy.backends.<name>]` table. The table key
// (the operator-chosen name) is the routing namespace; provider_type selects
// which provider handler serves the backend.
//
// A backend names its upstream credential through one of api_key, api_key_file,
// or api_key_env, any one of which satisfies the validate tags. Naming more than
// one is not yet rejected: only api_key and api_key_file exclude each other, and
// Resolve applies whichever sources are set in order, api_key_env last.
// TODO(TODO.d/auth-requirement-and-key-env-source.md): the auth field replaces
// these tags and makes the sources mutually exclusive.
type BackendSpec struct {
	ProviderType string `toml:"provider_type" validate:"required,oneof=openai"`
	BaseURL      string `toml:"base_url" validate:"required,url"`
	APIKey       string `toml:"api_key" validate:"required_without_all=APIKeyFile APIKeyEnv,excluded_with=APIKeyFile"`

	// APIKeyFile is the absolute path to a file whose trimmed contents Resolve
	// uses as the backend's key when set; Resolve rejects a relative path.
	APIKeyFile string `toml:"api_key_file"`

	// APIKeyEnv names an environment variable whose value Resolve uses as the
	// backend's key. It names the variable rather than interpolating its value so
	// that a declared credential stays distinguishable from a resolved one.
	APIKeyEnv string `toml:"api_key_env"`
}

// Resolve builds a [BackendConfig] from the parsed configuration. URL parsing
// happens here so the proxy receives validated *url.URL values at the
// type-system boundary.
func (c Config) Resolve() (*BackendConfig, error) {
	out := &BackendConfig{Backends: make(map[string]Backend, len(c.Backends))}
	for name, b := range c.Backends {
		u, err := url.Parse(b.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("understudy.backends.%s: invalid base_url %q: %w", name, b.BaseURL, err)
		}
		apiKey := b.APIKey
		if b.APIKeyFile != "" {
			if !filepath.IsAbs(b.APIKeyFile) {
				return nil, fmt.Errorf("understudy.backends.%s: api_key_file %q must be an absolute path", name, b.APIKeyFile)
			}
			data, err := os.ReadFile(b.APIKeyFile)
			if err != nil {
				return nil, fmt.Errorf("understudy.backends.%s: read api_key_file %q: %w", name, b.APIKeyFile, err)
			}
			apiKey = strings.TrimSpace(string(data))
			if apiKey == "" {
				return nil, fmt.Errorf("understudy.backends.%s: api_key_file %q is empty", name, b.APIKeyFile)
			}
		}
		if b.APIKeyEnv != "" {
			apiKey = os.Getenv(b.APIKeyEnv)
		}
		out.Backends[name] = Backend{
			ProviderType: b.ProviderType,
			Config:       providers.Config{BaseURL: u, APIKey: apiKey},
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if len(c.Models) > 0 {
		out.Models = make(map[string]LogicalModel, len(c.Models))
		for name, m := range c.Models {
			out.Models[name] = LogicalModel(m)
		}
	}
	return out, nil
}

// validate reports the first document rule the configuration breaks: a logical
// model with no targets, a target naming a backend the document does not
// declare, or a target whose overrides are unacceptable.
func (c Config) validate() error {
	for name, m := range c.Models {
		if len(m.Targets) == 0 {
			return fmt.Errorf("understudy.models.%s: no targets", name)
		}
		for _, t := range m.Targets {
			if _, ok := c.Backends[t.backend]; !ok {
				return fmt.Errorf("understudy.models.%s: target %q references unknown backend %q", name, t.identity(), t.backend)
			}
			if err := t.validate(); err != nil {
				return fmt.Errorf("understudy.models.%s: target %q: %w", name, t.identity(), err)
			}
		}
	}
	return nil
}

// DefaultModel returns the reserved default logical model when at least one
// backend is configured, or "" when configless (the agent then selects its
// own model).
func (c Config) DefaultModel() string {
	if len(c.Backends) == 0 {
		return ""
	}
	return DefaultLogicalModel
}
