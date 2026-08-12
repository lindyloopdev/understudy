# Honor an advertised backoff when nothing else is left

**Tag:** understudy / bug

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — an
advertised `readmitAt` supersedes the schedule, and a target carrying one is never
half-open-probed early. [DESIGN.md §Understudy](../DESIGN.md#understudy) — the
contribution table's "was benched and never called | its `readmitAt`, less now",
which is what such a list owes its client.

`pickTarget`'s exhausted-list fallback returns the last target its backend could
resolve, whatever its health — including one benched until an advertised moment that
has not arrived. Measured: a lone target demoted with `Retry-After: 60` is called
again one second later. That contradicts `pickTarget`'s own doc ("routed around
until that time … never half-open-probed early") and `recordRateLimited`'s promise
("that moment supersedes the recovery interval"). The call is spent
to be told what understudy already knows.

**Blocked, and the blocking is the point.** Removing the early call is a one-line
change that makes the answer worse. Measured today: that request answers `429`
carrying the upstream's own delay, because understudy calls the target and relays
what it says. With the target excluded from the fallback the list has nothing to
attempt and answers `404` "logical model has no targets" — a model that has targets,
all of them due back at a known time, told it has none.

The answer a benched list owes is the bench itself: a `429` carrying the soonest
`readmitAt`, less now. That is the contribution row in
[[weigh-every-candidates-contribution]], which waits on
[[understudy-adaptive-coordinated-backoff]] for something to weigh a bench against.
So this lands with that row, not before it.

Until then the early call stands, and the case beside it —
"should serve from a benched candidate rather than answer for an unusable one that
sorts after it" — pins it, because that case demotes with a `Retry-After`. When the
bench row lands, that case should demote *without* one, so it keeps driving the
schedule-held fallback it means to rather than the advertised bench.
