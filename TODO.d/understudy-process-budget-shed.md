# Bound and sharpen the process-budget shed's Retry-After

**Tag:** understudy / ratelimit / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the synthesized
backoff and the rate-limit reject; and
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) —
the FD budget as a process backstop.

## Remaining work

- **Floor it lower.** The first interval is `graduatedBackoffBase`, sized for a
  model swap rather than a passing squeeze; a blip should cost the client a
  near-instant retry instead.
- **Trip at "near" full**, not only 100% — proactive backpressure at a high-water
  threshold (CoDel's target), rather than waiting for the last slot to go.

Prior art: CoDel (time-above-target escalation, fast recovery), Google SRE overload
handling / adaptive throttling, EWMA/proportional control.

## Related

Shares the synthesize-jittered-capped-reset idiom with
[[understudy-adaptive-coordinated-backoff]] — this is its process-capacity analog and
the inherently cross-session-coordinated signal (the FD budget is process-wide).
[[understudy-limiter-ceiling-ratchet]] tracks the related deferred limiter/FD-budget
refinements.
