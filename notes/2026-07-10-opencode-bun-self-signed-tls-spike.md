# opencode-on-Bun trusts a self-signed understudy cert via `NODE_EXTRA_CA_CERTS`

**Date:** 2026-07-10
**Subject:** feasibility spike — can understudy serve its data plane over HTTPS
with a self-signed certificate that opencode (running on its bundled Bun runtime)
will trust, so the per-scene token and payload stop crossing the Docker bridge in
cleartext?
**Status:** feasible. Affirmative architecture lives in
[DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(transport encryption); the work is
[TODO.d/understudy-tls.md](../TODO.d/understudy-tls.md). This note preserves the
empirical result and the Bun-version caveats so the approach is not re-litigated.

## Why this was in doubt

understudy is HTTP-only today (`understudyBaseURL` returns `http://…/v1`). opencode
has no config-file key for a custom CA — its provider block exposes only
`baseURL`/`apiKey`/`options` (JSON), so a custom CA or custom `fetch` cannot be
injected there. The only supported path is the `NODE_EXTRA_CA_CERTS` env var, and
**Bun's** support for it has regressed repeatedly (bun#24581: on Linux, ignored
unless `NODE_USE_SYSTEM_CA=1` is also set; bun#13867: multi-cert bundle PEMs fail
where a single cert works; Bun's bundled undici ignores the patched CA store —
claude-code#25977). So it had to be proven against the actual opencode/Bun build,
not assumed.

## The spike

A throwaway Go HTTPS server presenting a self-signed leaf cert (SAN
`IP:127.0.0.1`) mimicked `/v1/models` and `/v1/chat/completions`. opencode
**1.15.13** was pointed at it with a provider config mirroring lindy's real
`opencode_config.go` shape (`@ai-sdk/openai-compatible`, `baseURL` =
`https://127.0.0.1:.../v1`, `apiKey = {env:LINDY_SESSION_KEY}`), then driven with
`opencode run`.

| Run | Env | Result |
| --- | --- | --- |
| Negative control | no CA env | opencode errors `self signed certificate`; **no request reaches the server** — verification is genuinely enforced |
| Positive | `NODE_EXTRA_CA_CERTS` + `NODE_USE_SYSTEM_CA=1` | `POST /v1/chat/completions` reaches the server over TLS |
| Minimal | `NODE_EXTRA_CA_CERTS` **alone** | also reaches the server — `NODE_USE_SYSTEM_CA=1` is **not** required on this build |

opencode's own log confirmed the real path (`provider=understudy … llm.runtime=ai-sdk … stream`), so the AI-SDK native-`fetch` request — not some incidental client — is what trusted the cert.

## Findings that shape the build

- **Minimal recipe:** `NODE_EXTRA_CA_CERTS=<single-cert.pem>`. Keep the CA a
  single-cert PEM (dodges bun#13867); keep `NODE_USE_SYSTEM_CA=1` as cheap
  insurance for other builds, but it is not required on 1.15.13.
- **opencode bundles its own Bun**, so the Bun version is pinned to the opencode
  release lindy ships. The Bun CA-handling fragility is therefore bounded by our
  opencode pin, not a moving target — but a bump can still regress it, so a
  startup smoke test (one HTTPS fetch, fail loudly) is worth its cost.
- **The insecure path is rejected:** `NODE_TLS_REJECT_UNAUTHORIZED=0` (and
  per-request `rejectUnauthorized:false`) disable verification entirely, reopening
  the shared bridge to a MITM — the exact threat this closes. Not used.

## Sources

- Bun v1.1.22 release (added `NODE_EXTRA_CA_CERTS`): https://bun.com/blog/bun-v1.1.22
- bun#24581 — `NODE_EXTRA_CA_CERTS` needs `NODE_USE_SYSTEM_CA=1` on Linux (open, Nov 2025): https://github.com/oven-sh/bun/issues/24581
- bun#13867 — bundle-PEM failure: https://github.com/oven-sh/bun/issues/13867
- claude-code#25977 — Bun's bundled undici ignores the patched CA store: https://github.com/anthropics/claude-code/issues/25977
- opencode#1694 — TUI honors `NODE_EXTRA_CA_CERTS` for a private CA: https://github.com/sst/opencode/issues/1694
