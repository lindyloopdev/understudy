# Rate-limit signal classifier: general base + thin per-host overrides

**Tag:** understudy / ratelimit

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

understudy's 429 handling grew up around z.ai — the *worst* case: a byte-identical
429 body for both concurrency limiting and quota exhaustion, no `Retry-After`, no
distinguishing code. That degenerate case must not set the policy for every
backend. Most providers emit real signals (`Retry-After`, the IETF
`RateLimit-Remaining`/`RateLimit-Reset` draft headers, `x-ratelimit-*`,
`anthropic-ratelimit-*`, provider quota codes like OpenAI `insufficient_quota` /
Gemini `RESOURCE_EXHAUSTED`); for those, reading the signal beats guessing.

Gemini is a middle case worth designing around: it emits a real signal, but only
in the response body (no `Retry-After` header). As of 2026-07-12 the OpenAI-compat
endpoint understudy uses passes the structured `RetryInfo.retryDelay` +
`QuotaFailure` details through, and the same timing also appears as message prose
(`Please retry in <float>s`). The body shape is **not stable** — an earlier capture
was prose-only — so the prose parse is kept as the resilient fallback. See
[notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md](../notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md).

## Decision: general classifier base + thin per-host overrides

- A **general signal engine** classifies a response by the strongest standard
  signal present. Handles the ~80% of conforming providers, and unknown/new
  providers gracefully.
- A **thin per-host override** layer supplies only what the engine can't infer —
  e.g. z.ai's "the body/headers are useless, so for a *signal-less* 429 fall back
  to the in-flight heuristic." The in-flight heuristic is thus a **per-host
  opt-in**, NOT a universal rule.
- Overrides live in **config** (per-backend in the resolved understudy config),
  with code defaults — riding the per-session-config / daemon direction
  ([[understudy-shared-daemon-subserver]]), so a new provider is a config entry,
  not a code change.

## The seam

Classification lives in one place — `classifyLimit(err) limitClassification`
(`understudy.go`), carrying `hasRetryAfter`/`retryAfter`/`shouldReject` plus a
`condition` (`limitCondition`). `errToResponse` reads the reject/forward/
synthesize fields; `chatCompletions` throttles the cap on the signal-less
condition and derives the demote from the `condition` plus the limiter's
in-flight count. The remaining work extends this seam.

## Build path

1. **Per-host override hook:** `classifyLimit` consults per-backend policy (config).

## Deferred (later, on this seam)

- Proactive rate-limit-header reading (`RateLimit-Remaining`/`Reset`,
  `x-ratelimit-*`, `anthropic-ratelimit-*`) — back off *before* a 429.
- Provider-specific quota/billing **code** parsing to sharpen the terminal kind
  beyond `Retry-After` ([[understudy-error-envelope-type]]).

## Related

The terminal/reject path is the stateless side of [[understudy-ratelimit-firewall]];
the transient-rate path connects to [[understudy-adaptive-coordinated-backoff]];
demote/fail-over feeds [[understudy-fallback]]. z.ai's signal-less 429 is a
provider degeneracy the general path shouldn't assume away.
