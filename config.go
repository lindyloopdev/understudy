package understudy

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/go-playground/locales/en"
	ut "github.com/go-playground/universal-translator"
	"github.com/go-playground/validator/v10"
	entranslations "github.com/go-playground/validator/v10/translations/en"

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

// Resolve builds a [BackendConfig] from the parsed configuration, applying
// [Config.Validate] before it reads anything. URL parsing happens here so the
// proxy receives *url.URL values at the type-system boundary.
func (c Config) Resolve() (*BackendConfig, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	out := &BackendConfig{Backends: make(map[string]Backend, len(c.Backends))}
	for name, b := range c.Backends {
		// The url tag rejects everything url.Parse does, and Validate ran it above,
		// so a parse failure here is unreachable.
		u, _ := url.Parse(b.BaseURL)
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
	if len(c.Models) > 0 {
		out.Models = make(map[string]LogicalModel, len(c.Models))
		for name, m := range c.Models {
			out.Models[name] = LogicalModel(m)
		}
	}
	return out, nil
}

// tagValidator checks the per-field constraints declared in the validate struct
// tags, reporting each field by its TOML key so a violation names what the
// operator wrote rather than the Go identifier behind it. tagTranslator renders
// those violations as English sentences.
var tagValidator, tagTranslator = func() (*validator.Validate, ut.Translator) {
	eng := en.New()
	// GetFallback rather than GetTranslator("en"): the fallback is the same
	// English locale and is returned unconditionally, so there is no lookup that
	// could miss and no found-flag worth discarding.
	trans := ut.New(eng, eng).GetFallback()
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		return fld.Tag.Get("toml")
	})
	if err := entranslations.RegisterDefaultTranslations(v, trans); err != nil {
		panic("understudy: registering validation translations: " + err.Error())
	}
	// The stock required_without_all sentence reads "api_key is a required
	// field", which is false — naming api_key_file or api_key_env satisfies it.
	if err := v.RegisterTranslation("required_without_all", trans,
		func(ut.Translator) error { return nil },
		func(_ ut.Translator, fe validator.FieldError) string {
			return fmt.Sprintf("%s is required unless %s is set", fe.Field(), alternatives(fe))
		},
	); err != nil {
		panic("understudy: registering the credential-source translation: " + err.Error())
	}
	return v, trans
}()

// backendKeys maps BackendSpec's Go field names to the TOML keys an operator
// writes. A tag names its sibling fields in Go, but a message about them has to
// speak the operator's vocabulary, and BackendSpec is the only spec whose fields
// a tag names.
var backendKeys = func() map[string]string {
	spec := reflect.TypeFor[BackendSpec]()
	keys := make(map[string]string, spec.NumField())
	for f := range spec.Fields() {
		if key := f.Tag.Get("toml"); key != "" {
			keys[f.Name] = key
		}
	}
	return keys
}()

// alternatives renders a tag's sibling field names the way an operator reads
// them — TOML keys, joined by "or" — for a rule whose message must say which
// other fields would satisfy it. A name from outside BackendSpec renders as the
// Go identifier: wrong-looking in a message about a TOML file, which is the
// point — it shows up rather than vanishing.
func alternatives(fe validator.FieldError) string {
	names := make([]string, 0, 2)
	for param := range strings.FieldsSeq(fe.Param()) {
		names = append(names, cmp.Or(backendKeys[param], param))
	}
	return strings.Join(names, " or ")
}

// Validate reports the first rule the configuration breaks — the per-field
// constraints in the validate tags, then the rules spanning the whole
// configuration. [Config.Resolve] applies it before reading anything, so a
// caller need not remember to; calling it directly answers the question earlier,
// against a configuration still being assembled or about to be written.
func (c Config) Validate() error {
	if err := tagValidator.Struct(c); err != nil {
		if fieldErrs, ok := errors.AsType[validator.ValidationErrors](err); ok && len(fieldErrs) > 0 {
			return fieldViolation(fieldErrs[0])
		}
		return err
	}
	return c.validate()
}

// fieldViolation renders a tag failure the way understudy reports its own: the
// TOML path the operator wrote, then what they must change. The validator's
// default rendering names the tag that fired, which says nothing to someone
// editing a config file.
func fieldViolation(fe validator.FieldError) error {
	// Namespace is "Config.backends[groq].api_key"; the operator's path is what
	// follows the struct name, with the map key unbracketed.
	path := strings.NewReplacer("[", ".", "]", "").Replace(fe.Namespace())
	_, path, _ = strings.Cut(path, ".")
	section := strings.TrimSuffix(path, "."+fe.Field())
	return fmt.Errorf("understudy.%s: %s", section, fe.Translate(tagTranslator))
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

// DefaultModel returns [DefaultLogicalModel] when at least one backend is
// configured, or "" when configless (the agent then selects its own model).
// Nothing here checks that a logical model of that name is declared, and routing
// gives the name no special treatment — a consumer that requests it against a
// configuration that never declared it is answered like any other unknown
// logical model.
//
// TODO(TODO.d/remove-the-reserved-default-model.md): departs with
// [DefaultLogicalModel] — which of understudy's logical models a consumer
// requests is the consumer's to decide, from its own configuration.
func (c Config) DefaultModel() string {
	if len(c.Backends) == 0 {
		return ""
	}
	return DefaultLogicalModel
}
