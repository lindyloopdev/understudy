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

- **Rank a session by the caller's session identifier.** DESIGN.md's
  *Admission ordering is oldest-session-first*: where a request carries
  `x-session-affinity`, its session's age keys on that, scoped to the token,
  so a compacted session keeps its place in line.
- **Finish the first-content timeout** (DESIGN.md, *A first-content
  timeout*). What remains:
  - This composes with [[stall-before-first-content-event]]'s demote-and-
    replay-to-a-different-candidate disposition, which answers a different
    question (which target) for the same triggering silence; how the two
    decide precedence is unsettled.
- **Answer a session's first request at once when every target is unavailable
  for new sessions** (DESIGN.md, *Who waits, and who fails over*).
- **Admit around a target that a 429 with `Retry-After` benches** (DESIGN.md,
  *A 429 on a request that waits re-queues it*). Blocked on the wait budget's
  `T_wait` ([[understudy-fallback]],
  [[understudy-adaptive-coordinated-backoff]]).
- **Linked models** (DESIGN.md, *The queue is keyed on the target*). A request
  re-queued by the 429 rule counts progress across the linked set.
- **Operator guidance.** Kronk's `QueueDepth` (DESIGN.md, *Backend
  configuration*) belongs in [[documentation]].

## Outside this repo

- Lindy dispatches the `400` of type `upstream_unavailable` — resend at
  `retry_after_ms`, route the beat elsewhere, or abort — and sends the
  session-end signal.
- Lindy chooses a beat's target from its own history of beat durations — the
  per-session routing the design leaves to the consumer.
