# Answer a busy backend's 503 as retryable, not as a failure

**Priority:** high
**Tag:** understudy / fallback / multi-model

**Design:**
[DESIGN.md §Understudy](../DESIGN.md#understudy) — the stall dispositions (case 1
is this, "the busy-local-model case"), the availability walk, the synthesized
backoff, and the rate-limit reject; and
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting).
Related: [[understudy-adaptive-coordinated-backoff]]. Consumer context:
[notes/2026-08-08-kronk-gemma4-local-stack.md](../../../lindy/notes/2026-08-08-kronk-gemma4-local-stack.md)
in the lindy repo.

## Remaining work

**Pin the health exemption with a test.** Nothing but statement order keeps the
busy branch ahead of `recordFailure`, so a refactor can start spending the streak
silently. Assert that a busy target accrues none.

**Bound the condition** with a wall-clock budget rather than an attempt count, so
a backend that is not swapping but broken degrades to an honest terminal failure
instead of being told to come back forever. Size it against
`maxPassthroughRetryAfter` rather than inventing a value. This also covers the one
other producer of `code: "unavailable"` — kronk's `mid/authen.go`, when an external
auth service is down, which is persistent rather than transient.

**Jitter the interval,** so concurrent requests against one backend do not retry
in lockstep. It stays a "don't hammer" value, **not** an estimate of swap time —
swap duration is per-GPU and per-model, and nothing can know it. Coverage of a long
swap comes from repetition, not from sizing one delay, and it must stay under
`maxPassthroughRetryAfter` so it rides the passthrough rather than tripping the
reject.

Keep genuine 5xx on the existing 502/demote path — the discriminator is
"capacity" vs "failure".

## Out of scope

kronk's **429s** get no such treatment. `FromSDK` maps two unrelated conditions
to `ResourceExhausted` → 429: `ErrNoCapacity` ("insufficient memory budget" — the
model does not fit, permanent) and `ErrAdmissionTimeout` (already waited out the
3m admission budget). Neither is a transient-busy signal, and retrying the first
would spin forever. The existing rate-limit path is an acceptable answer for both.
