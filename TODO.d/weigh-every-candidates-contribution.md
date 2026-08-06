# Weigh every candidate's contribution, not the first throttle

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs
out of candidates answers for the request, not for its last target", the
contribution table beneath it (what each disposition offers), and "the verdict is
the soonest contribution, answered in the shape of the candidate that made it".

Nothing pins that the comparison weighs what *remains* of each offer rather than
what each advertised: every case compares candidates at one virtual instant, so a
remembered delay and a re-derived one agree. A walk where time passes between calls
tells them apart — `a` advertising 40s, `b` taking 15s to answer and advertising
30s, ending on a refusal: the client is owed `a`'s remaining 25s, not `b`'s 30s.
The stub's sleep has to stay under the 20s header-stall gate.

A walk ending on a target unusable as configured still discards an earlier
throttle; a refusal and a `5xx` no retry can help both yield to one.

The remaining contribution rows are unbuilt too: a benched candidate's
`readmitAt`, a stall's synthesized backoff, and a retryable failure advertising
nothing (which needs [[understudy-adaptive-coordinated-backoff]] to have an
interval to offer).

**Open first:** whether a candidate left untried because the walk stopped
contributes at all. §Understudy names benched candidates as the ones counted
beyond those tried; a transient `429` ends the walk with later targets never
called, and nothing says whether they are the request's candidates for this
purpose. It decides what the comparison ranges over, so settle it before building
the comparison.
