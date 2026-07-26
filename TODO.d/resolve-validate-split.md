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
`api_key_file` resolves here too — expand a leading `~/` against the home
directory and an otherwise-relative path against the config file's directory,
replacing today's rejection of any non-absolute path and the test pinning it.

**Two kinds of rule, split by what an absent credential means.** `auth` is the
discriminator (§Credential requirement): a defect in the document, or a fact
about the world.

- **Document validity** — a target naming an unknown backend, a logical model
  with no targets, more than one key source, a reserved value (`optional`,
  `thinking = true`), an empty key under `auth = "required"`. No degraded mode
  answers these: loading a document carrying one fails, for every embedder alike.
- **Availability** — an unset variable under `auth = "auto"`. Not a rule outcome
  at all: the document is correct and the world is not supplying a key. It seeds
  the health state the failover walk consults, per
  [[auth-requirement-and-key-env-source]].

## Build path

1. Move `Target`'s rules out of `Resolve` — `t.validate()` is a document rule, not
   part of filling in a target. `Target.UnmarshalText` already parses at decode
   time, so only the rules move.
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
- **`base_url` stays a `string`.** Giving it a type with `UnmarshalText` moves the
  failure to decode time, which reads like an improvement and is not: the
  string-only `required,url` tag is what enforces an absolute URL with a scheme,
  and a type can only enforce what `url.Parse` rejects — which accepts
  `api.openai.com`, `/just/a/path`, `not a url`, and `""`. The trade buys a key
  path in one error message and costs the rule itself, plus an exported-API break.

## Open

- **Are the document rules also callable on a config held in memory?** Loading
  runs them regardless, so no embedder can skip them. Exposing them additionally
  buys a `check`/lint path that reports every problem at once rather than the
  first, and lets lindy validate a config it assembled before writing it. The
  reference pattern (`pth/admin/main/backend/validate`) has the caller invoke a
  helper that runs tags then `Validate()`.
