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

## The gap

A busy 503 replays onto a sibling, but with no sibling to reach it falls through
to the generic 5xx path: `clientFacing` renders **502**, and `isFatalUpstream`
spends the failure streak `terminalFailure` converts. That is the shape the
motivating case has — a single-member logical model (`review-local-12b` → one
kronk target), where the caller gets `"upstream unavailable"` for a backend that
is merely swapping models. Observed: four review beats fail within a second of
each other, every run.

## Remaining work

**Relay the 503 with a synthesized `Retry-After` when no candidate remains,**
per §Understudy's synthesized-backoff rule, rather than converting the status.
opencode retries any 5xx and honors `Retry-After` regardless of status
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

**Exempt that path from target health too.** The exemption holds only where the
replay does; a busy 503 with nowhere to go still reaches `recordFailure`.

**Pin the exemption with a test.** Nothing but statement order keeps the busy
branch ahead of `recordFailure`, so a refactor can start spending the streak
silently. Assert that a busy target accrues none.

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
