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
- **Pin the headered case of growth's hold-on-unconfirmed check.** Growth
  holds an arrival while anything at its target is unconfirmed past grace;
  `TestShouldSendOrHoldARequestOnArrival` covers the headerless occupant
  only. Add the headered counterpart as a row there:
  a fresh, non-outranking arrival is held without ever reaching the target
  while a *headered* request at that target is unconfirmed past its grace
  period (a target admitting to its own internal queue before serving, the
  way Kronk can) — header state must not matter to this check, only
  confirmation state.
- **Growth bursts freely again once a formed queue has drained.** A burst
  of arrivals, none yet unconfirmed past its grace period, is pinned to pass
  straight through; nothing pins that the same holds after a queue has
  formed and drained.
  - Tests: once a formed queue has drained — every waiter admitted and
    resolved, nothing left unconfirmed past its grace period — a later
    arrival passes straight through again.
- **A stuck refill answers to the same rules as a stuck growth attempt.**
  Refilling is not an exemption from anything downstream of admission — a
  refilled request that fails to confirm is preemptable by an outranking
  arrival and subject to the first-content timeout exactly like a growth attempt
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
