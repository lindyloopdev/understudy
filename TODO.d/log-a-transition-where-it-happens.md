# Log a transition where it happens

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A transition is logged
when it happens", and the health-transition exemption above it, which is what allows
understudy its own log line at all. [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
— demotion and half-open re-admission, the states these records report.

`noteBackendDown` fires from `pickTarget` for every demotion but a stall: not when a
target is demoted, but when some later request's walk routes around it. Everything
wrong with those records follows from that one placement.

- The **cause** is out of scope, so the reason is re-derived from `readmitAt` and
  can only ever name a schedule. A target refused outright reads `awaiting recovery
  probe`, which is true of when it will be retried and says nothing of why it is out.
- The **error** is out of scope, so the record cannot say what the backend answered.
- The **moment** is out of scope: the record is dated when a walk tripped over the
  target, which is why it carries `failing_since` at all.
- A demotion **nobody trips over is never logged**.

Move `recordRateLimited` and `recordImmediateFailure` to emit as `recordStalled`
does. Every call to them is made with the cause, the error, and the moment in hand,
so each names one reason with no inference, and nothing needs carrying forward on
`targetHealth`.

The two emission sites must also agree on what a record says: `recordStalled`'s
carries no `failing_since`, `pickTarget`'s does. Settle that as the last path moves,
rather than leaving an operator to notice which code wrote which line.

**Two emitters is the end state, so the record needs one home.** The walk keeps
emitting for a streak that ages in silence — DESIGN sanctions it as the earliest
knowable moment — so the demotion sites and `pickTarget` both write "backend down"
for good. Today each composes the record inline and they already disagree, one
carrying `failing_since` and one not. Give the record a single constructor, so its
message, attrs, and the pairing of a reason with its schedule cannot drift.

Both also claim the same `downLogged` under `s.mu` and emit after releasing it, so
for a target that already carries an entry, which reason an operator reads — the
stall's own, or one the walk derived — follows lock order. That narrows as the paths
move, since the walk's remaining case has an unambiguous reason, but it does not
vanish: a target aging in silence can still stall while a walk is routing around it.

**Which cause a record names is open.** `recordStalled` announces for any entry whose
demotion was never logged, so a target benched by a `429` and later stalling is
reported `stall backoff` — the cause it is out for now, rather than the one that
started the streak. That is the same choice [[say-why-a-backend-went-down]] leaves
open for the error, and both should be answered once, together.

Three behaviors go unasserted until then: **should name the cause a target is out for
now, not the one that started its streak**; **should announce a stall on a target
already demoted by something else**; and **should stay silent when a target stalls
twice in one streak** — `recordStalled`'s update path is driven by no test at all.

**The emission still may not happen under `s.mu`.** These paths write health through
`writeHealth`, which holds the lock for the whole write, so logging from inside them
puts a consumer's handler back under the lock every other request's walk contends
for — the fault fixed in 364782a. The record has to be composed where the cause is
known and emitted after the lock is released, as `pickTarget` already does.

A streak that merely ages past the failover threshold has no such moment — nothing
runs when the threshold elapses. Where a further failure does the crossing, that
failure is the event; where the streak ages in silence, the discovering walk stays
the emission point, and dates the streak rather than the discovery.

Absorbs the record's other two open faults, both of which exist only because of the
placement: [[say-why-a-backend-went-down]] (the error is in hand at the demotion) and
[[report-when-a-target-actually-started-failing]] (the demotion knows the real
moment, so it need not publish the backdate the walk relies on).

Guards: "should say an upstream's advertised backoff holds a target back" and "should
say understudy's own probe pacing holds a target back" both pin reasons that must
survive; the transition-count cases pin once-per-streak, which the demotion must
still honor.
