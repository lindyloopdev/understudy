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
- **A stuck refill answers to the same rules as a stuck growth attempt.**
  Refilling is not an exemption from anything downstream of admission — a
  refilled request that fails to confirm is preemptable by an outranking
  arrival and subject to the first-content timeout exactly like a growth attempt
  that fails to confirm.
  - Tests: a refilled request that goes unconfirmed past its grace period is
    displaced by an outranking arrival the same way a growth attempt would
    be.
