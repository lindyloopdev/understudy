# Say why a backend went down

**Tag:** understudy / feature

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — a demoted
target is routed around and re-admitted by a half-open probe, which is the schedule
the "backend down" line reports. [DESIGN.md §Understudy](../DESIGN.md#understudy) —
the request record's `Excluded` attempts, which are where the failing error is kept
today.

The "backend down" line says which target, why it is being routed around
(`upstream retry-after`, `no response header`, or `probe not yet due`), when its
streak is measured from, and when it comes back. It does not say what the backend
answered when it failed, so an operator reading it cannot tell a credential problem
from an outage.

The error is not lost — `addLogCalled` puts it on the demoting request's
`Excluded[].Err`. But that is a different record, and finding it means identifying
which request demoted the target, which is exactly what the operator lacks.

The error needs no carrying: every call to `recordFailure`, `recordImmediateFailure`,
`recordRateLimited`, and `recordStalled` is made with it in hand. So this lands
where each demotion is written, and what remains here is only what the record should
say of it: its status and message alongside the reason.

One thing to settle when it lands:

- **Which error, when a streak has several.** The one that started the streak names
  the original fault; the most recent names what is happening now. They differ
  exactly when a target's failure mode changes mid-streak, which is the case worth
  reading.
