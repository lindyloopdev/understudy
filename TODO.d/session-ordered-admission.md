# Session-ordered admission with probe discipline

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — the ordering rule,
engagement, probe discipline, and verdicts;
[DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and admission*
(session identity) and *Stalls* (the dispositions the verdicts refine);
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting)
— the account cap this composes with rather than replaces.

Multi-turn sessions against a capacity-constrained backend (one local decode
stream) interleave at request granularity today: per-turn latency inflates by
session count, all sessions finish together, and the consumer's silence-based
watchdog aborts queued sessions as dead. Measured locally: queue waits p50
2.4s / p90 60s against a 10-minute consumer idle watchdog.

Not blocked on [[stall-before-first-content-event]] or
[[wait-not-replay-on-bounded-backends]]: those change the disposition of a
comment-only stall, which on a marked backend nets back to today's wait. This
work needs only mechanism it builds itself — the backend marker, first-content
detection for promotion, and exempting a parked probe from the header gate —
and whichever of those lands later must leave marked backends waiting.

Headers mean an admission permit, not a slot. kronk (v1.32.3) waits for its
permit before writing anything — the `200` included — and the permit pool is
`NSeqMax × QueueDepth` (default 1 × 2), so:

- a request waiting for a permit is **byte-silent**: once the permits are
  held, a parked probe gets no header, and today's header gate
  (`defaultHeaderStallGate`, 20s) demotes the target and replays it;
- a request with headers can still wait in the engine queue behind
  keep-alives before prefill, so headers cannot confirm — promotion needs the
  first content event.

Settled decisions:

- **Keying.** The admission queue is keyed on the canonical **base URL** —
  the server is the scarce resource whatever credential reaches it — and one
  queue is deliberately shared across *models* served by that server, since
  the GPU, not the model entry, is what contends. Distinct from the account
  cap's `(url + key)` and health's `(url + key + model)`.
- **Engagement.** The discipline engages exactly on a bounded-admission
  backend marker — not on a runtime contention detector and never on unmarked
  (opaque/remote) backends. A queue then forms under demand on marked
  backends; nothing serializes a parallel remote. There is no disengagement
  logic and no capacity model anywhere.
- **Session identity.** Queue keys use the conversation key from first turn
  on — affinity deliberately engages only after a prior assistant turn, but
  ordering by session age requires the first turn to create the session
  (`conversationKey` already computes one). Two requests in flight on one key
  is a collision (two containers, same charter and first turn — the eval-rig
  shape): the second arrival is a different conversation and queues; it is
  never an incumbent.
- **Priority is `first_seen`.** Each conversation key records when it was
  first seen, and that age alone ranks a turn — a session between turns
  (running tools, nothing at the backend) keeps its rank, so its next turn
  outranks every younger session's.
- **Preemption only before headers.** An older session's turn displaces the
  parked probe only while that probe has received no response headers — kronk
  has done no work on it, so the cancel is free. A probe with headers is never
  canceled, prefilling or not: the older turn waits at the front of
  understudy's queue until the probe's first content event confirms it, then
  parks as the next probe. The wire cannot tell engine-queue wait from prefill
  (one keep-alive ticker covers both), so this errs toward not preempting; the
  reservation window ([[admission-reservation-window]]) absorbs most of the
  cost.
- **No failover on queue wait.** A request held in understudy's queue
  failovers on *error* only; healthy-busy waits. The backend's own admission
  bound resolves the head's fate, and the consumer's patience deadline (not
  understudy) bounds the rest — a queue-wait failover would preempt the
  routing decision the consumer owns.
- **Verdict states.** `held` (in understudy's queue, position — no bytes
  sent, so byte-silence must not read as dead), `waiting` (submitted,
  comment-only), `progressing` (content events), `dead` (silence or stall
  per disposition).

## Work

- **Backend marker** — a field on the backend config declaring that this
  backend queues requests itself and enforces its own admission bound (a
  local model server). Threaded through config.go. Learning the property from
  observation is a later, backed refinement; the marker is the v1 source.
- **Queue + probe discipline** on marked backends: session-keyed queue
  ordered by `first_seen`; exactly one unconfirmed request (the queue head)
  outstanding at the backend; confirmed requests run undisturbed; promotion
  on first content event (reading past SSE comment lines to find it); an
  older session's turn cancels a parked probe that has no headers and submits
  itself; a request with headers is never canceled; the head going away
  upstream cancels and re-parks.
- **Header gate exemption** — a parked probe on a marked backend is not
  demoted or replayed for a missing header; the backend's own admission
  timeout, surfacing as an error, resolves it.
- **Verdict surface** as a library API (this repo has no control plane;
  lindy's daemon serves it): a snapshot of in-flight requests keyed by the
  gateway-assigned request-record id — the same id the requestlog join
  already smuggles, since a consumer cannot recompute the conversation key
  (the wire messages are template-wrapped) and a per-token view cannot
  attribute which of a token's sessions is waiting. States per above.
- Tests against a fake constrained backend: interleaved sessions complete in
  arrival order; an older session's turn displaces a headerless probe; a
  probe with headers is never displaced; an admitted request is never
  canceled; a headerless parked probe outlasting the header gate is neither
  demoted nor replayed; a colliding second request on one key queues;
  unmarked backends never form a queue; held requests report `held`, not
  `dead`; the snapshot joins on the record id.
