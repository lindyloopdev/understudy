# Log a transition where it happens

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A transition is logged
when it happens", and the health-transition exemption above it, which is what allows
understudy its own log line at all. [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
— demotion and half-open re-admission, the states these records report.

`noteBackendDown` fires from `pickTarget`: not when a target is demoted, but when
some later request's walk routes around it. Everything wrong with the record follows
from that one placement.

- The **cause** is out of scope, so the reason is re-derived from `readmitAt` — a
  field two paths set. A pre-header stall, which advertised nothing, is reported as
  `advertised backoff`: the opposite of what happened.
- The **error** is out of scope, so the record cannot say what the backend answered.
- The **moment** is out of scope: the record is dated when a walk tripped over the
  target, which is why it carries `failing_since` at all.
- A demotion **nobody trips over is never logged**.

Move the emission to the demotion. Every call to `recordStalled`,
`recordRateLimited`, and `recordImmediateFailure` is made with the cause, the error,
and the moment in hand, and each names one reason with no inference. Nothing needs
carrying forward on `targetHealth`.

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
