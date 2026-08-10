# Report when a target actually started failing

**Tag:** understudy / bug

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion
and the threshold a streak is measured against, which is the value the backdate
manipulates.

`demotedHealth` backdates `failingSince` to `now - failoverThreshold` so `pickTarget`
routes around a demoted target on the very next request. That is a routing device,
but the "backend down" record publishes the same field as `failing_since`, documented
as when the streak began. Every immediately-demoted target — an access refusal, a
pre-header stall, an advertised `429` — therefore tells an operator it started
failing a full threshold before its first request ran. Measured: a target whose only
failure is at `00:00:00` is logged `failing_since 1999-12-31T23:59:45Z`.

Separate the two: keep whatever makes the walk route around the target immediately,
and report the moment it actually failed. A demotion moment distinct from the streak
start is one way; so is a flag that says "route around this now" without moving the
clock.

The behavior to pin: **should say when a target actually started failing, not when
its demotion is measured from.** No case asserts `failing_since` on a demoted target
today — the one that did was pinning this artifact and was removed rather than
codify it.

Pre-existing: the backdate predates this branch. What is new is that the record now
publishes it.
