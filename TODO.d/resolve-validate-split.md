# Separate loading from validation

**Tag:** understudy / refactor / config

**Design:** [DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(the two-stage pipeline, and the `auth` rules validation will carry),
[DESIGN.md §Understudy](../DESIGN.md#understudy) (logical models and targets,
whose rules move into the validation stage).

Config handling is load then validate, and understudy has neither stage cleanly.
`Config.Resolve` decodes, dereferences credentials, *and* rejects malformed
documents in one pass, while the `validate` struct tags express a partial second
set of rules the library never runs — an embedder must build its own validator.

**Loading** — decode the TOML, parse `base_url` into a `*url.URL` and each target
into its type, fill each backend's key from the source it names. Fails only where
loading is impossible: a malformed document, an unreadable path. Reading a file or
an environment variable to populate a field belongs here, not in a separate phase.

**Validation** — every rule about whether the loaded configuration is acceptable:
the credential is present (per `auth`), exactly one source is named, a relative
`api_key_file`, the `thinking` override, a target naming an unknown backend, a
logical model with no targets.

## Build path

1. Move `url.Parse` and `Target`'s rules out of `Resolve`. `Target.UnmarshalText`
   already parses at decode time — `base_url` should do the same, which also gives
   a malformed URL the TOML key path in its error.
2. Move the document rules to tags where tags reach, and `Validate() error` methods
   where they don't — a target naming an unknown backend is a cross-map reference
   no tag can express; `Target`'s fields are unexported so tags cannot see them.
3. Reduce `Resolve` to filling credentials, or fold it into loading entirely and
   decide what remains of the `Config`/`BackendConfig` split.

## Constraints

- **Loading must not discard the source fields.** Validation reports *which*
  variable is unset, which is the sentence that makes a `free-tiers.toml` failure
  actionable. Keeping one struct through validation preserves it for free;
  transforming into a credential-only type would throw away exactly that fact.
- **Nothing here prunes.** A backend whose credential did not load stays in the
  configuration; `auth = "auto"` makes it *unavailable*, not absent — see
  [[auth-requirement-and-key-env-source]].

## Open

- **Does the library run validation, or expose it?** It does neither today, which
  is how the tags drifted out of step with `api_key_env`. The reference pattern
  (`pth/admin/main/backend/validate`) has the caller invoke a helper that runs tags
  then `Validate()`, which argues for exposing and letting `cmd/understudyd` and
  lindy each call it.
- **`api_key_file` relative paths.** DESIGN.md promises `~/` expansion and
  config-relative resolution; the code rejects any non-absolute path and a test
  pins that. One of them changes, and config-relative paths would give a
  distributed config a second usable credential source.
