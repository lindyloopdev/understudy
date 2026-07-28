# Drop-in free-tier config + the daemon wiring it needs

**Tag:** understudy / documentation / release

**Design:** [DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(the `auth = "auto"` drop this file relies on),
[DESIGN.md §Understudy](../DESIGN.md#understudy) (the failover the demo exercises).

The out-of-box experience the public release is judged on:

```
go install github.com/lindyloopdev/understudy/cmd/understudyd@latest
export GEMINI_API_KEY=...
understudyd -config free-tiers.toml
```

An unedited file, one key set, a working failover chain.

- `examples/free-tiers.toml` — every backend `auth = "auto"` + `api_key_env`,
  grouped into capability tiers (`fast`, `smart`), `default` → `fast`. Comment
  each backend with signup URL, limit shape, and last-verified date, so the
  startup report is self-explanatory to someone enabling one. Overlaps the entry
  in [[documentation]]; this item owns the file, that one owns the README.
- `cmd/understudyd` reports the backends that started **unavailable** for want of
  a credential as **one** INFO summary line naming the unset variables — five
  separate lines on a first run reads as something being wrong. INFO, not DEBUG:
  "why isn't groq being used?" should not require raising the log level. This is a
  report about availability, not about the config: every backend is still
  configured.
- Exit with `no backends have credentials: set one of GEMINI_API_KEY, …` when none
  is usable. This is a daemon-level judgement that serving nothing is pointless,
  not a config rejection — the library keeps accepting the configuration, and a
  configless understudy stays legal.

Depends on [[auth-requirement-and-key-env-source]] and the daemon in
[[standalone-daemon]].

Deferred, all in `cmd/` rather than the library: multiple `-config` files (or a
`conf.d/`) for augmenting the shipped file — the out-of-box user has no config of
their own to augment, so this is a second-session need; a TOML `include`
directive, which needs two different merge rules (backends are a map, a model's
targets concatenate); and remote fetch of the file, which needs signing or
pinning before it ships, since a backend entry controls `base_url` and a tampered
file redirects traffic and whatever credential is bound to it.
