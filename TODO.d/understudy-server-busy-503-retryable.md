# Treat upstream 503 server-busy as retryable (not 502/terminal)

**Priority:** high
**Tag:** understudy / fallback / multi-model

**Design:**
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting).
Related: [[understudy-model-groups]] (admission scheduler / per-request wait
budget) and [[understudy-adaptive-coordinated-backoff]]. Consumer context:
[notes/2026-08-08-kronk-gemma4-local-stack.md](../../../lindy/notes/2026-08-08-kronk-gemma4-local-stack.md)
in the lindy repo.

## The gap (verified 2026-08-09)

A backend's "I'm temporarily full" signal — kronk's `503 server busy: no idle
pool entry available` when a request targets a non-resident model while another
is generating on a single-GPU pool — is currently treated as a **generic 5xx
failure**, not a capacity signal:

- `understudy.go:912` — `sig.isRateLimit = sig.status == http.StatusTooManyRequests`:
  only **429** enters the rate-limit/retry-after/hold path.
- `understudy.go:818` — `if status >= 500 { … }`: any 5xx (incl. this 503) is
  rendered as **502 Bad Gateway** to the caller and counts against target health
  (demotion).
- `understudy.go:944` — `case !sig.isRateLimit`: the non-429 branch, i.e. a bare
  503 is not retried gracefully.

So kronk's 503 → 502 to lindy/opencode → a non-fatal (`?`) review beat is
**dropped, not retried**. This is especially bad when the shedding backend is the
**only** target (single kronk server): demotion leaves failover nowhere to go, so
the request goes terminal instead of waiting out the capacity burst.

## Why it matters

lindy runs a **mix of models on one GPU** through kronk (gpt-oss, gemma-12b, e4b
— only one resident at a time). kronk does not queue cross-model requests
(verified: it 503s by design, "the client should retry later"), and kronk has no
config to change that. So the queueing/retry **must** live in understudy for
fire-and-forget mixed-model runs to work; without it, beats that hit kronk while
another model is active are lost.

## Fix direction

Distinguish a capacity 503 from a real upstream failure and route it into the
retry path:

- Recognize kronk's server-busy 503 — by its body/message (`server busy: no idle
  pool entry available`), and/or by honoring a `Retry-After` header if kronk
  emits one (kronk-side companion change to set `Retry-After` would make this
  clean and backend-agnostic).
- Treat it as **retryable against the same backend** with backoff (bounded, per
  [[understudy-adaptive-coordinated-backoff]]) rather than demote-and-failover —
  critical for the single-backend case where failover has nowhere to go. Hold the
  request up to a wait budget (see [[understudy-model-groups]] §admission
  scheduler) rather than 502'ing immediately.
- Keep genuine 5xx (backend actually broken) on the existing 502/demote path —
  the discriminator is "capacity" vs "failure".

Open question: hold-and-retry inline (understudy absorbs the wait) vs return a
retryable response to the caller. Inline hold matches the user-facing
"fire-and-forget" goal; it needs the per-request wait budget so a saturated
backend can't park unbounded work.
