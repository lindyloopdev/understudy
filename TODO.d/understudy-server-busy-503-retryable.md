# Answer a busy backend's 503 as retryable, not as a failure

**Priority:** high
**Tag:** understudy / fallback / multi-model

**Design:**
[DESIGN.md §Understudy](../DESIGN.md#understudy) — the availability walk, the
synthesized backoff, and the rate-limit reject; and
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting).
Related: [[understudy-adaptive-coordinated-backoff]]. Consumer context:
[notes/2026-08-08-kronk-gemma4-local-stack.md](../../../lindy/notes/2026-08-08-kronk-gemma4-local-stack.md)
in the lindy repo.

## The gap

The provider recognizes kronk's busy signal and raises `providers.ErrServerBusy`
alongside the upstream's 503, but the core does not consult it. So a "I'm
momentarily full" 503 — kronk's answer when a request names a non-resident model
while another is generating on a single-GPU pool — still travels the generic 5xx
path:

- `clientFacing` renders it **502** to the caller.
- `isFatalUpstream` counts it against target health.
- Nothing retries it.

With a single-member logical model (`review-local-12b` → one kronk target),
demotion leaves failover nowhere to go, so `terminalFailure` converts the streak
and the caller gets `"upstream unavailable"`. Observed: four review beats fail
within a second of each other, every run.

## Remaining work

**Relay the 503 with a synthesized `Retry-After`,** per §Understudy's
synthesized-backoff rule, rather than converting the status. opencode retries any
5xx and honors `Retry-After` regardless of status
([notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md](../notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md)),
so a 429 would buy nothing and would assert something untrue. Two things block it:
`clientFacing` maps every 5xx to 502, and `errToResponse` emits the `Retry-After`
header only for a 429. The second is not specific to this signal — injecting a
backoff on a 5xx at all belongs to [[understudy-adaptive-coordinated-backoff]],
which owns it; this entry needs it to land, not to define it.

Keep the interval under `maxPassthroughRetryAfter` so it rides the existing
passthrough rather than tripping the reject. It is a "don't hammer" value, **not**
an estimate of swap time — swap duration is per-GPU and per-model, and nothing can
know it. Coverage of a long swap comes from repetition, not from sizing one delay.
Jitter it, so concurrent requests against one backend do not retry in lockstep.

**Exempt it from target health.** Without this the rest does nothing: the 5xx
still feeds the failure streak and `terminalFailure` still converts it.

**Bound it** with a wall-clock budget rather than an attempt count, so a permanent
condition degrades to an honest terminal failure instead of spinning. Size it
against `maxPassthroughRetryAfter` rather than inventing a value. This also covers
the one other producer of `code: "unavailable"` — kronk's `mid/authen.go`, when an
external auth service is down, which is persistent rather than transient.

Keep genuine 5xx on the existing 502/demote path — the discriminator is
"capacity" vs "failure".

## Out of scope

kronk's **429s** get no such treatment. `FromSDK` maps two unrelated conditions
to `ResourceExhausted` → 429: `ErrNoCapacity` ("insufficient memory budget" — the
model does not fit, permanent) and `ErrAdmissionTimeout` (already waited out the
3m admission budget). Neither is a transient-busy signal, and retrying the first
would spin forever. The existing rate-limit path is an acceptable answer for both.
