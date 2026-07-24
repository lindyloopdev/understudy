# v0.x known limitations (note in README)

Accepted for v0 — document these in the README rather than block the release
on them:

- `Config`/`BackendSpec`/`LogicalModelSpec` carry TOML tags — the library is
  TOML-oriented for v0; YAML/JSON embedders see the leak.
- `Config` (spec) → `BackendConfig` (resolved) breaks the `*Spec` naming
  pattern the other types follow.
- `RequestMetadata` lacks `upstream_status` — the library doc says it should
  carry it, so this is a code/doc disagreement (a latent bug worth promoting to
  a fix, not merely a caveat).
- `ErrInvalidToken` is a string sentinel, not a typed error.
