# Session-ordered admission

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — ordering, when a queue
forms, the probe discipline, the 429 rule, the consumer's time limit, and Kronk
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

Each item below is one deployable step, in order. The queue sits in the request
path ahead of the stall gate and the concurrency limiter.

## Work

- **A request with no response header waits.** Remove the header gate
  (`callWithHeaderGate`, `defaultHeaderStallGate`) and, on the chat path, the
  transport's `ResponseHeaderTimeout` (`providers/providers.go`). A request
  waits for its header until the backend errors or the client goes away.
  Kronk's permit wait and Ollama's queue, prefill, and model load are all silent
  before the header, and the 20s gate demotes and replays them.
  - Tests: a target silent past the old gate is served by that target, not
    demoted or replayed; a target that errors while silent takes the ordinary
    failure path.
- **Session age.** Record `first_seen` per tenant and conversation key from a
  conversation's first turn (affinity keys only after a prior assistant turn;
  ordering needs the first). A session keeps its age through any quiet stretch.
  Forget it on the consumer's session-end signal (new library surface, shared
  with [[admission-reservation-window]]) or after a backstop of about a day with
  no request. A request with no conversation key ranks by its own arrival.
  - Tests: a session returning after a long tool run keeps its rank; a session
    ended by the consumer, or idle past the backstop, returns ranked as new.
- **The queue.** Keyed on `url + key + model` (`healthKey`). Requests go straight
  through until a queue forms; a queue forms when an arrival outranks an
  outstanding headerless request past the grace period (cancel and queue every
  such request ranked below it), or when a request waits past the grace period
  with no header (cancel and queue every headerless request except the
  highest-ranked unconfirmed one, and that one too if another request is already
  unconfirmed). The grace period is a named constant of a second or two. The
  head is released only when nothing unconfirmed is outstanding on its key; a
  request is confirmed by its first content event; a request with a response
  header is never canceled by preemption; the queue drains back to
  straight-through. Confirmation needs the read-ahead past SSE comments and
  leading JSON whitespace that [[stall-before-first-content-event]] also needs —
  whichever lands first builds it, and the client's status and headers are
  withheld until the first content.
  - Tests against a fake contended backend: interleaved sessions complete in
    arrival order; an older session's turn displaces a headerless probe past its
    grace period; a request whose header arrived is never displaced; a request
    within its grace period is not canceled; headerless requests ranked above
    the arrival stay outstanding; an uncontended backend never forms a queue;
    a consumer aborting a held request removes it; the queue drains back to
    straight-through.
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
  progress across the linked set. The link ends when either queue drains.
  - Tests: an older session's request for the unloaded model is served before a
    younger session's request for the loaded one; the loaded model's running
    requests are never canceled for the swap; the swap request is sent only
    once no request for the loaded model is running.
- **The concurrency limiter follows the queue.** A held request takes no slot
  (`upstreamLimiter.acquire` runs after release), and a freed slot goes to the
  waiter from the oldest session rather than whichever wakes first.
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
