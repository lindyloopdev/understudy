# Grow the process-budget shed Retry-After with sustained saturation

**Tag:** understudy / ratelimit / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

When the process-wide FD budget is exhausted, `chatCompletions` sheds the request
immediately with `503` + a **fixed** `Retry-After` (`processBudgetRetryAfter`, 5s) —
no server-side queue, so no waiter accumulation. The fixed delay is the placeholder;
make it track how long/how hard the process has been saturated.

## Remaining work

Drive `Retry-After` from a process-wide saturation signal so a brief blip yields a
tiny backoff (near-instant client retry) and sustained pegging yields a longer one:

- Track saturation over time on the process limiter — a `saturatedSince` timestamp
  (set when utilisation crosses a high-water mark, cleared on recovery) in the
  `failingSince` idiom, or an EWMA of utilisation.
- `Retry-After = f(elapsed)` — exponential from `saturatedSince`, with a small
  **floor** (blip → ~instant retry).
- **Trip at "near" full**, not only 100% — proactive backpressure at a high-water
  threshold (CoDel's target).

Prior art: CoDel (time-above-target escalation, fast recovery), Google SRE overload
handling / adaptive throttling, EWMA/proportional control. Internal precedent:
`failingSince` duration-based backoff.

## Required guards

- **Cap it short** — keep the synthesized delay well under the reject threshold in
  [[understudy-ratelimit-firewall]], or a long shed Retry-After becomes the very hang
  the firewall exists to prevent. This is backpressure, not a bench.
- **Jitter** — a process-wide signal shed to every over-budget client at once will
  thundering-herd the retry without per-client jitter.
- **Reset/decay on recovery** — a recovered process snaps back to the floor.

## Related

Shares the synthesize-jittered-capped-reset idiom with
[[understudy-adaptive-coordinated-backoff]] — this is its process-capacity analog and
the inherently cross-session-coordinated signal (the FD budget is process-wide). The
shed enforcement this backoff rides on is already in `chatCompletions`;
[[understudy-limiter-ceiling-ratchet]] tracks the related deferred limiter/FD-budget
refinements.
