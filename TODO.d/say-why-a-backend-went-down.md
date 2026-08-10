# Say why a backend went down

**Tag:** understudy / feature

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — a demoted
target is routed around and re-admitted by a half-open probe, which is the schedule
the "backend down" line reports. [DESIGN.md §Understudy](../DESIGN.md#understudy) —
the request record's `Excluded` attempts, which are where the failing error is kept
today.

The "backend down" line says which target, why it is being routed around
(`advertised backoff` / `awaiting recovery probe`), when its streak began, and when
it comes back. It does not say what the backend answered when it failed, so an
operator reading it cannot tell a credential problem from an outage.

The error is not lost — `addLogCalled` puts it on the demoting request's
`Excluded[].Err`. But that is a different record, and finding it means identifying
which request demoted the target, which is exactly what the operator lacks. Carry
the demoting error onto `targetHealth` (`recordFailure`, `recordImmediateFailure`,
`recordRateLimited` all have it in hand) and log its status and message alongside
the reason.

Two things to settle when it lands:

- **Which error, when a streak has several.** The one that started the streak names
  the original fault; the most recent names what is happening now. They differ
  exactly when a target's failure mode changes mid-streak, which is the case worth
  reading.
- **Whether a health record should hold a client-supplied error at all.** The map is
  per-target and long-lived, so a retained error keeps whatever its wrapping
  references alive until the entry is evicted — store the status and message rather
  than the error value if that is a concern.
