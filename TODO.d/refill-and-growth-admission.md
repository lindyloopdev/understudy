# Refill and growth admission

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *How many requests a
target holds*, *Gating refills on the same grace period as growth would drain
a wide, slow target*, *A refill can be briefly wrong, and the rules that
already exist bound it*, and *Connections accepted are not capacity*.

Admission decides two different questions. **Growth** — whether to try for
more than a target has proven — admits a new arrival while nothing outstanding
is unconfirmed past its grace period, and holds it otherwise. **Refill** —
immediately re-admitting a replacement for a slot a confirmed request just
vacated — admits the next-ranked held request unconditionally, with no grace
check and no regard for anything else unconfirmed at the target.

## Work

- **Test that two confirmed requests ending close together each independently
  and immediately admit their own replacement, neither waiting on the other's
  header or confirmation** — the case DeepSeek/wide-backend behavior depends
  on, staged with a doubled target slow enough to answer that gating on header
  latency would visibly serialize the two.
- **A completion with nothing held admits nothing, and remembers nothing.**
  If the wait list is empty when a slot frees, there is no counter or belief
  left behind for a later arrival to consume — that arrival is a fresh
  arrival, subject to growth's ordinary grace-based check, not an automatic
  admission just because a slot recently opened. This is a deliberate choice:
  the alternative (remembering that a slot is "owed" until something claims
  it) was considered and rejected, because a target can already burst-admit
  freely while nothing has proven a limit — nothing is lost by not
  remembering, and remembering would reopen the kind of state the serving
  window was removed for.
  - Tests: a request arriving after a slot freed with nobody waiting, at a
    target where something else is simultaneously unconfirmed past its grace
    period, is held exactly as if nothing had recently freed — it is not
    admitted early just because a slot opened moments before it arrived.
- **Pin the headered case of growth's hold-on-unconfirmed check.** Growth
  holds an arrival while anything at its target is unconfirmed past grace;
  its test covers the headerless occupant only. Add the headered counterpart:
  a fresh, non-outranking arrival is held without ever reaching the target
  while a *headered* request at that target is unconfirmed past its grace
  period (a target admitting to its own internal queue before serving, the
  way Kronk can) — header state must not matter to this check, only
  confirmation state.
- **Growth still bursts freely while nothing has proven a limit.** Multiple
  simultaneous arrivals, none of which has yet gone unconfirmed past its
  grace period, should all pass straight through — this may already hold by
  accident of the current code (nothing currently stops it), but nothing
  pins it.
  - Tests: several requests arriving close together, all still within their
    individual grace periods, are all admitted immediately with no queueing
    among them; once a formed queue has drained — every waiter admitted and
    resolved, nothing left unconfirmed past its grace period — a later
    arrival passes straight through again.
- **A stuck refill answers to the same rules as a stuck growth attempt.**
  Refilling is not an exemption from anything downstream of admission — a
  refilled request that fails to confirm is preemptable by an outranking
  arrival and subject to the bound on silence exactly like a growth attempt
  that fails to confirm.
  - Tests: a refilled request that goes unconfirmed past its grace period is
    displaced by an outranking arrival the same way a growth attempt would
    be.
- **Refill correctly skips a departed waiter.** The head of the wait list may
  already have had its own client disconnect while held
  ([[session-ordered-admission]]'s existing departed-client handling). Refill
  pulling from that list must move on to the next entry rather than stalling
  on one that was already dropped.
  - Tests: a confirmed request's completion, with the head of the wait list
    already abandoned by its own client, admits the next request behind it
    rather than nothing.
