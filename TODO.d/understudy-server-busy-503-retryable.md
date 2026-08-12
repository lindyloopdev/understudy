# Treat upstream 503 server-busy as retryable (not 502/terminal)

**Priority:** high
**Tag:** understudy / fallback / multi-model

**Design:**
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting).
Related: [[understudy-adaptive-coordinated-backoff]]. Upstream findings:
[notes/kronk.md](../notes/kronk.md). Consumer context:
[notes/2026-08-08-kronk-gemma4-local-stack.md](../../../lindy/notes/2026-08-08-kronk-gemma4-local-stack.md)
in the lindy repo.

## The gap (line refs verified 2026-08-10)

A backend's "I'm temporarily full" signal — kronk's `503` when a request targets
a non-resident model while another is generating on a single-GPU pool — is
treated as a **generic 5xx failure**, not a capacity signal:

- `understudy.go:1025` — `sig.isRateLimit = sig.status == http.StatusTooManyRequests`:
  only **429** enters the rate-limit/retry-after path.
- `understudy.go:931` — `if status >= 500 { … }`: any 5xx (incl. this 503) renders
  as **502** to the caller and counts against target health via `isFatalUpstream`.
- `understudy.go:1057` — `case !sig.isRateLimit`: a bare 503 is not retried.

With a single-member logical model (`review-local-12b` → one kronk target),
demotion leaves failover nowhere to go, so `terminalFailure` converts the streak
and the caller gets `"upstream unavailable"`. Observed: four review beats fail
within a second of each other, every run.

## Fix

**Recognize** by content signature, in the openai provider alongside
`withGeminiQuotaRetryAfter` — the established idiom for a vendor dialect. The
signature is `503` **and** `code == "unavailable"`, which is exact rather than
heuristic: kronk's `errs.FromSDK` has one producer for that code
(`kronkpool.ErrServerBusy`, the busy-eviction sentinel), pinned by kronk's own
`errs_test.go`. Prefer it over matching the message prose. No config field, no
`provider_type`, no dialect axis — a signature needs no operator action and works
for anyone pointing at kronk without knowing it.

**Answer** the caller with a retryable response rather than holding the request:
render as 429 with a short jittered `Retry-After`, under
`maxPassthroughRetryAfter` so it rides the existing passthrough and opencode's
own retry does the waiting. Understudy then holds no connection and grows no
retry loop, and no wait budget is needed to stop a saturated backend parking
unbounded work.

The interval is a "don't hammer" value, **not** an estimate of swap time — swap
duration is per-GPU and per-model, and nothing can know it. Coverage of a long
swap comes from repetition, not from sizing one delay.

**Exempt it from target health.** Without this the fix does nothing: the 5xx
still feeds the failure streak and `terminalFailure` still converts it.

**Bound it** with a wall-clock budget rather than an attempt count, so a
permanent condition degrades to the honest 503 instead of spinning. Size it
against `maxPassthroughRetryAfter` rather than inventing a value. This also
covers the one other producer of `code: "unavailable"` — `mid/authen.go`, when an
external auth service is down, which is persistent rather than transient.

Keep genuine 5xx on the existing 502/demote path — the discriminator is
"capacity" vs "failure".

## Out of scope

kronk's **429s** get no such treatment. `FromSDK` maps two unrelated conditions
to `ResourceExhausted` → 429: `ErrNoCapacity` ("insufficient memory budget" — the
model does not fit, permanent) and `ErrAdmissionTimeout` (already waited out the
3m admission budget). Neither is a transient-busy signal, and retrying the first
would spin forever. The existing rate-limit path is an acceptable answer for
both. See [notes/kronk.md](../notes/kronk.md) §2.
