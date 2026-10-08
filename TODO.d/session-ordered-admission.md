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
  over*): the typed `400` when every target is unavailable for new sessions —
  the row is [[understudy-error-envelope-type]]'s.
  - Tests: with every target unavailable for new sessions, a first turn gets
    the typed `400` at once.
  - Check a waiting first request again when another target stops being busy.
    A target unavailable for new sessions at the crossing gets no further
    look when it frees up, so the request stays in the busy target's line
    while a first request arriving after is sent to the freed target. There
    is no known moment to check at, so the freed target's queue has to say
    when it stops holding arrivals. (open) How many waiting first requests
    move to it at once — all of them could make it busy again straight away;
    growth's one-at-a-time rule is the likely answer.
    - Tests: a first request waiting at a busy target moves to the other
      target, busy at the crossing, once it stops being busy.
  - The target's busy clock starts when the target became unable to take an
    arrival. A request that arrived to wait counts from its own arrival — the
    request's clock, not the target's — which sets the crossing once the
    earliest waiter leaves before it. (open) Which moment the remaining busy
    period began then.
    - Tests: a first turn waiting when the earliest waiter leaves before the
      crossing moves on at the crossing the target's remaining busy period
      names.
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
- **Linked models** (DESIGN.md, *The queue is keyed on the target*). Refused
  again, the request waits for the next of the server's running models to
  finish. Rank the waiting requests of every model on the server together,
  and while the top-ranked waiter is for a model the server refused, stop the
  server's running models taking new requests — refill included, since refill
  admits on every completion (*How many requests a target holds*) and would
  keep a running model from ever finishing. A request re-queued by the 429
  rule counts progress across the linked set.
  - Tests: with two models running, a refused request that needs the server
    to itself is refused again when the first finishes and sent once both
    have finished; an
    older session's request for a refused model is served before a younger
    session's request for a running one; the running models' requests finish
    undisturbed; a running model takes no new request while a higher-ranked
    waiter for a refused model is pending; a request refused as busy is held
    only for requests on its own account, not one sharing the address under
    another API key.
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
