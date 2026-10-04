# Session-ordered admission

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — ordering, when a queue
forms, refill and growth, the 429 rule, the consumer's time limit, and Kronk
configuration; [DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and
admission* (session identity, the coherence-driven wait budget) and the
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

- **Reclaim a dropped tenant's conversation state at teardown.** The forget
  backstop drops a quiet conversation's record only after
  `sessionForgetAfter` — up to a day after a run ends — and a run's token
  never returns, so nothing else names the state again. The consumer's
  session-end signal, when it lands (new library surface, shared with
  [[admission-reservation-window]]), needs a hook the embedding daemon's
  tenant registry can call so the state goes at teardown instead.
  - Tests: a tenant dropped by the registry leaves no conversation state
    behind once its last request has ended.
- **Finish the first-content timeout.** DESIGN.md's *A first-content
  timeout* is built for one request at a time: an attempt with no content
  within its timeout is canceled and sent again to the same target, on its own
  timer. The timeout is 60s, doubling with each re-send it causes at that
  target; a displacement leaves it where it was, and a failover's new target
  starts again at 60s. What remains:
  - Take the first value from the consumer's own time limit when one is
    supplied, rather than the 60s default sized from lindy's
    notes/2026-09-30-time-to-first-content-measurements.md.
  - When it fires on a target that gives no work before its header, every
    headerless request at that target restarts together, in rank order, so
    the retry restores ordering rather than scrambling it.
  - A mass-retry after such a restart follows growth's ordinary rule — burst
    together, not serialized one at a time — since nothing about a retry
    changes what growth already knows to do with a fresh admission.
  - (open) Whether a new arrival accelerates the drop of an already-wedged
    *headered* request, or only the timeout's own independent timer ever
    resolves it, is undecided.
  - This composes with [[stall-before-first-content-event]]'s demote-and-
    replay-to-a-different-candidate disposition, which answers a different
    question (which target) for the same triggering silence; how the two
    decide precedence is unsettled.
  - Tests: several headerless requests at one target, all wedged together,
    restart together and are re-admitted per growth's burst rule rather than
    one at a time.
- **Who waits, and who fails over** (DESIGN.md, *Who waits, and who fails
  over*): the rest of a target unavailable for new sessions — first turns
  already waiting move on, and the clock clears — and the typed `400` when
  every target is — the row is [[understudy-error-envelope-type]]'s.
  - Tests: a first turn already waiting moves when the threshold passes; a
    first turn goes to the first
    target again once its queue would accept an arrival, with no arrival in
    between; with every target unavailable for new sessions, a first turn gets
    the typed `400` at once.
- **A 429 on a request that waits re-queues it** at its rank — a later turn,
  or a first turn whose target is still taking new sessions. With
  `Retry-After`: bench the target (the existing `recordRateLimited`) and admit
  the highest-ranked request whose target is not benched. Without `Retry-After`,
  while another request on its key is producing content: re-queue, admit it
  when a request on its key finishes, and do not `throttle()` the limiter.
  Without `Retry-After` and nothing producing content: the ordinary ladder.
  Kronk's admission timeout is a bare `429` `resource_exhausted`, identical to
  its model-does-not-fit error (`notes/kronk.md` #2), so the rule must not key
  on the code.
  - Tests: a bare 429 while its key has a running stream is served once that
    stream ends, with the cap unchanged and no replay; a 429 with `Retry-After`
    holds only its own target while others are admitted; a bare 429 with nothing
    running takes the ladder.
- **Linked models.** A busy refusal (`providers.ErrServerBusy`) links the queues
  of every model on that server into one ranking. While linked, the loaded
  model's queue stops admitting when the top-ranked waiter is for another model,
  and that waiter is admitted when Understudy's count of the loaded model's
  running requests reaches zero. A request re-queued by the 429 rule counts
  progress across the linked set. The link ends when either queue drains. This
  must suspend refill for the loaded model too, not only growth — refill admits
  unconditionally on a completion (DESIGN.md §Session-Ordered Admission, *How
  many requests a target holds*), so without this carve-out it would keep
  feeding the loaded model a replacement every time one finishes and its running
  count would never reach zero, and the swap would never happen.
  - Tests: an older session's request for the unloaded model is served before a
    younger session's request for the loaded one; the loaded model's running
    requests are never canceled for the swap; the swap request is sent only
    once no request for the loaded model is running; a request waiting for the
    loaded model itself is not refilled while a higher-ranked waiter for the
    other model is pending.
- **The concurrency limiter follows the queue.** Keep a held request taking no
  slot, and route a freed slot to the waiter from the oldest session rather
  than whichever wakes first — `upstreamLimiter.acquire` wakes its waiters in
  release order.
  - Tests: with the cap full, a freed slot goes to the oldest session's
    waiting request.
- **The consumer's time limit.** An optional library setting (Lindy supplies
  it, as it supplies the response interceptor): a request that has produced no
  content within the limit — held, or submitted — is canceled and answered with
  a typed `400` carrying the reason (behind older sessions, or waiting on a model
  swap), its position, and a retry-after hint. The envelope type is
  [[understudy-error-envelope-type]]'s row. Unset, Understudy waits. A resent
  conversation keeps its rank.
  - Tests: a held request past the limit is answered with the typed `400` and
    its position; a submitted request with a header but no content past the
    limit is canceled at the backend and answered the same way; with no limit,
    the request waits; a resent conversation returns at its original rank.
- **Operator guidance.** Kronk's `QueueDepth` (keep the default of 2; a depth of
  1 needs `AdmissionTimeout` raised past the longest turn) belongs in
  [[documentation]].

## Outside this repo

- Lindy dispatches the still-queued `400` and the no-target-taking-new-sessions
  `400` (resend later, route the beat elsewhere, or abort) and sends the
  session-end signal.
- Lindy chooses a beat's target from its own history of beat durations — the
  per-session routing the design leaves to the consumer.
