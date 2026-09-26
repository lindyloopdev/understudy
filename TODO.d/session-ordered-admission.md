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

- **Forget a session's age on the consumer's session-end signal** (new library
  surface, shared with [[admission-reservation-window]]). Until it lands,
  `sessionAges` holds one entry per conversation ever seen — a stale entry is
  reset rather than dropped.
  - Tests: a session ended by the consumer returns ranked as new.
- **The bound on silence is unbuilt.** DESIGN.md's *A bound on silence, keyed
  on content* replaces the old "no gate bounds the wait for a header": a
  request producing no content within the bound is canceled and sent again to
  the same target, whether or not its header arrived, so a submitted request
  is never silently unbounded regardless of contention. None of it exists yet.
  - The bound fires on its own timer, independent of any arrival — a lone
    wedged request with nobody else contending at that target must still
    eventually retry, since nothing else will ever trigger a recheck of it.
  - When it fires on a target that gives no work before its header, every
    headerless request at that target restarts together, in rank order, so
    the retry restores ordering rather than scrambling it.
  - The bound escalates with each attempt on one request, so a legitimately
    slow first attempt is not retried forever at an interval shorter than it
    could ever succeed within.
  - A mass-retry after such a restart follows growth's ordinary rule — burst
    together, not serialized one at a time — since nothing about a retry
    changes what growth already knows to do with a fresh admission.
  - (open) Whether a new arrival accelerates the drop of an already-wedged
    *headered* request, or only the bound's own independent timer ever
    resolves it, is undecided.
  - This composes with [[stall-before-first-content-event]]'s demote-and-
    replay-to-a-different-candidate disposition, which answers a different
    question (which target) for the same triggering silence; how the two
    decide precedence is unsettled.
  - Tests: a submitted request producing no content for longer than the bound,
    with nothing else contending for its target, is eventually canceled and
    retried with nobody else involved; several headerless requests at one
    target, all wedged together, restart together and are re-admitted per
    growth's burst rule rather than one at a time; a retry's interval is
    longer than the previous attempt's.
- **Order turns already waiting at the target.** DESIGN.md's *What forms a
  queue* has a request waiting past its grace period form the queue, so turns
  sent together — before anything at the target was past its grace period —
  and now waiting at the backend in its own order are still served oldest
  session first. The wait trigger built for it (`formByWaiting`) is
  unreachable: `targetQueue.enter` arms its timer only for an arrival beside a
  request already unconfirmed past its grace period, and growth holds every
  such arrival before it reaches that code. It never covered turns sent
  together in the first place.
  - Tests: two turns sent to one target within its grace period, the younger
    session's first, both waiting there unanswered past it — the older
    session's turn is served first.
- **Test the probe-held arm of the waiting trigger** (waits on the item
  above: the trigger must be reachable first). No case stages a
  headered unconfirmed request ranked below a headerless one when the wait
  forms the queue, so the arm that queues the probe because preemption never
  takes a headered request is unexercised. This is the sweep's own
  cancellation logic (`formByWaiting`), distinct from growth's admit-check
  above — the admit-check decides whether a *new* arrival is let in; this
  decides who the sweep takes once the wait trigger has already fired.
  - Tests: the highest-ranked unconfirmed request at a target is taken by the
    sweep, not spared as the probe, when a different, headered request is the
    one currently unconfirmed at that target.
- **Withhold status and headers until the first content.** The relay commits
  the upstream's status and headers as they arrive; hold them until the
  confirmation read-ahead (past SSE comments and leading JSON whitespace)
  finds content.
  - Tests against a fake contended backend: a request whose header arrived is
    never displaced; a request within its grace period is not canceled;
    headerless requests ranked above the arrival stay outstanding; an
    uncontended backend never forms a queue; a consumer aborting a held
    request removes it; the queue drains back to straight-through.
- **Two untested nuances of preemption, both already true of the code.**
  First: a preempting arrival waits behind *every* unconfirmed request at its
  target once it is queued, not only the ones it outranked — the release rule
  is "nothing unconfirmed," not "nothing older unconfirmed," so an arrival that
  only displaced some of the outstanding set still waits for the rest,
  regardless of rank. Second: an outranking arrival is not dispatched the
  instant it displaces a younger request — it is itself queued alongside the
  displaced one and released only once that cancellation actually completes,
  not in the same instant.
  - Tests: an arrival that outranks only some of several simultaneously
    outstanding requests is answered only after every one of them — including
    the ones it did not outrank — has resolved; a displaced request's
    replacement reaches the target no sooner than the cancellation it
    displaced actually completes, never in the same instant.
- **Who waits, and who fails over.** A later turn, and a first turn with no
  healthy untried alternate, wait in the queue. A first turn with a healthy
  alternate takes the walk and fails over rather than queue.
  - Tests: a first turn with an alternate is served by the alternate while the
    preferred target is contended; a later turn waits for its own target; a
    first turn with no alternate waits.
- **A 429 on a request that waits re-queues it** at its rank. With
  `Retry-After`: bench the target (the existing `recordRateLimited`) and release
  the highest-ranked request whose target is not benched. Without `Retry-After`,
  while another request on its key is producing content: re-queue, release it
  when a request on its key finishes, and do not `throttle()` the limiter.
  Without `Retry-After` and nothing producing content: the ordinary ladder.
  Kronk's admission timeout is a bare `429` `resource_exhausted`, identical to
  its model-does-not-fit error (`notes/kronk.md` #2), so the rule must not key
  on the code.
  - Tests: a bare 429 while its key has a running stream is served once that
    stream ends, with the cap unchanged and no replay; a 429 with `Retry-After`
    holds only its own target while others release; a bare 429 with nothing
    running takes the ladder.
- **Linked models.** A busy refusal (`providers.ErrServerBusy`) links the queues
  of every model on that server into one ranking. While linked, the loaded
  model's queue stops releasing when the top-ranked waiter is for another model,
  and that waiter is released when Understudy's count of the loaded model's
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

- Lindy dispatches the still-queued `400` (resend later, route the beat
  elsewhere, or abort) and sends the session-end signal.
- Lindy chooses a beat's target from its own history of beat durations — the
  per-session routing the design leaves to the consumer.
