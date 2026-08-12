# Say what an accrued streak answered

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A transition is logged
when it happens", and the one shape it exempts: a streak that ages past the threshold
in silence, reported by the walk that discovers it.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion and half-open
re-admission, the states that record reports.

`targetHealth.lastError` is set by the three paths that demote at once, and not by
`recordFailure`, which opens a streak without demoting. A streak that ages past the
failover threshold is therefore reported by the walk with `upstream_error` empty — the
one record whose reader has no request of their own to look at, since no request
demoted the target.

`recordFailure`'s call site holds the same error the other three pass.

**Should say what a target answered when a walk discovers its streak.**
