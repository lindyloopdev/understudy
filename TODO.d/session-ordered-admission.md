# Session-ordered admission

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — ordering, when a queue
forms, refill and growth, the 429 rule, and Kronk configuration;
[DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and admission*
(session identity, the coherence-driven wait budget) and the
`Retry-After` ladder; [DESIGN.md §Concurrency & Rate
Limiting](../DESIGN.md#concurrency-rate-limiting) — where the admission queue
sits relative to the learned cap.

Multi-turn sessions against a contended backend interleave at request
granularity: per-turn latency inflates by session count, all sessions finish
together, and the consumer's silence-based watchdog aborts them all. Measured
locally: queue waits p50 2.4s / p90 60s against a 10-minute consumer idle
watchdog, with Kronk turns (ornith 1.5) of 6–10 minutes.

The queue sits in the request path ahead of the stall gate and the concurrency
limiter. The steps below remain, in order.

## Work

- **Finish the first-content timeout.** DESIGN.md's *A first-content
  timeout* is built: an attempt with no content within its timeout is canceled
  and sent again to the same target, together with every other request there
  that has no response header. The timeout is 60s, doubling with each re-send
  it causes at that target; a displacement leaves it where it was, and a
  failover's new target starts again at 60s. What remains:
  - Take the first value from the target's own turnover — its observed time
    to first content ([[log-request-timings]]) — rather than the 60s default
    sized from lindy's notes/2026-09-30-time-to-first-content-measurements.md.
  - This composes with [[stall-before-first-content-event]]'s demote-and-
    replay-to-a-different-candidate disposition, which answers a different
    question (which target) for the same triggering silence; how the two
    decide precedence is unsettled.
- **Who waits, and who fails over** (DESIGN.md, *Who waits, and who fails
  over*): when every target is unavailable for new sessions, answer a first
  turn at once with a `400` of type `upstream_unavailable` and
  `retry_after_ms`, as the nothing-left answer does; why the targets are
  unavailable goes to the log.
  - Tests: with every target unavailable for new sessions, a first turn gets
    the `400` `upstream_unavailable` at once.
- **A 429 with `Retry-After` on a request that waits benches the target** —
  the existing `recordRateLimited` — **and admits the highest-ranked request
  whose target is not benched**, instead of taking the ladder. Blocked on the
  wait budget: holding a request through a `Retry-After` is the Retry-After
  ladder's wait-in-place rung, which applies only within the budget, and
  `T_wait` is set from measured held-connection cost
  ([[understudy-fallback]], [[understudy-adaptive-coordinated-backoff]]);
  until it exists, such a 429 takes the fail-over rung. A bare 429 with
  nothing running takes the ordinary ladder. Kronk's admission timeout is a
  bare `429` `resource_exhausted`, identical to its model-does-not-fit error
  (`notes/kronk.md` #2), so no rule may key on the code.
  - Tests: a 429 with `Retry-After` holds only its own target while others are
    admitted; a bare 429 with nothing running takes the ladder.
- **Linked models** (DESIGN.md, *The queue is keyed on the target*). A request
  re-queued by the 429 rule counts progress across the linked set.
- **Operator guidance.** Kronk's `QueueDepth` (keep the default of 2; a depth of
  1 needs `AdmissionTimeout` raised past the longest turn) belongs in
  [[documentation]].

## Outside this repo

- Lindy dispatches the `400` of type `upstream_unavailable` — resend at
  `retry_after_ms`, route the beat elsewhere, or abort — and sends the
  session-end signal.
- Lindy chooses a beat's target from its own history of beat durations — the
  per-session routing the design leaves to the consumer.
