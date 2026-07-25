# Credential requirement vocabulary + `api_key_env` source

**Tag:** understudy / config

**Design:** [DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(credential sourcing, the `auth` vocabulary),
[DESIGN.md §Understudy](../DESIGN.md#understudy) (a logical model resolves to a
priority-ordered target list, which the drop prunes).

Build the `auth` requirement declaration and the third key source it depends on.

- Add `api_key_env` to `BackendSpec` — names an environment variable, mutually
  exclusive with `api_key`/`api_key_file`. An unset or empty variable is
  unresolved, matching `api_key_file`'s existing empty-contents rule.
- Add `auth` (`required` default / `none` / `auto`, plus `optional` reserved).
  This replaces the `validate:"required_without=APIKeyFile"` tag, which makes a
  keyless backend contract-illegal today — the tag is the reason a local ollama
  entry cannot currently be expressed.
- Reject `auth = "optional"` at `Resolve` with a reserved-value error, following
  the `thinking=true` precedent in `target.go`, and carry the matching inline
  `TODO(TODO.d/…)` marker.
- Under `auth = "auto"`, `Resolve` drops an unresolvable backend, drops targets
  naming it, and drops logical models left with no targets — then reports the
  skipped backend names (a field on `BackendConfig`; `Resolve`'s signature stays).
- Reject the invalid combinations: a key source declared under `none`; no source
  declared under `required` or `auto`.

Blocked on [[empty-logical-model-panic]] — the model drop is what makes an empty
target list ordinary rather than hand-written.

The library never runs the validator (`go-playground` appears only in
`config_test.go`), so the `validate` tags are a contract embedders enforce. Any
tag change here is a contract change lindy sees; relaxing a constraint keeps
existing configs valid, so the two sides need not land together.

Deferred: `api_key`/`api_key_file`/`api_key_env` as three mutually-exclusive
fields needs three pairwise exclusions, and a fourth source would need six — the
shape is asking to become one `[backends.<name>.key]` sub-table with a source
discriminator. Not worth restructuring at v0; noted so the next source added
reconsiders rather than adding a fourth field.
