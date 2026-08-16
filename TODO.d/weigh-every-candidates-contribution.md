# Tell a client when something will serve, whatever turned it away

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs
out of candidates answers for the request, not for its last target", the
contribution table beneath it (what each disposition offers), and "the verdict is
the soonest contribution, answered in the shape of the candidate that made it".

When nothing can serve a request now, the client should be told when something
will. The dispositions the walk replays past deliver on that today — a timed
backoff, and a rate limit with no delay carrying the interval synthesized from its
streak — each weighed against every other, yielding to a refusal or an
unimplemented operation. The rest of the table does not, so which promise a client
gets still depends on what happened to end its walk.

**This is one behavior, not a row per disposition.** The verdict is a comparison,
so a contributor is only worth building when everything it can be compared against
is also computable. The rows below are blocked on each other, not independent:

- A **benched candidate** offers its `readmitAt`. Only reachable when the walk ends
  without replaying — a plain `5xx` from a fresh pick, or a transient `429` —
  because `untriedTargets` ignores health, so any replay calls the benched
  candidate anyway. Measured: with `[a benched, b: 401]` the walk calls `a`, so
  "declined to call" is far rarer than §Understudy implies.
- A **plain `5xx` from a fresh (within-threshold) pick** offers that endpoint's
  synthesized interval, but never replays, so it ends the walk as the last failure
  rather than contributing to the verdict — and takes an earlier throttle's sooner
  return down with it, since the verdict displaces the last failure only for a
  refusal or a `501`. This blocks the bench row: the walk that leaves a bench
  uncalled usually ends on a `5xx`, so the comparison has nothing to weigh the
  bench against. A demoted target's due half-open probe replays a `5xx` onto the
  next untried target instead, scoping this row to the fresh-pick case.
- A **stall** offers the synthesized stall backoff.
- A walk ending on a target **unusable as configured** still discards an earlier
  throttle. Reachability unverified: `pickTarget` skips such targets rather than
  ending a walk with their error.

So the `5xx` row comes next, then the rest of the table together. Building the
bench row alone would honor the promise in one corner and break it in the common
case, which is worse than breaking it everywhere.

**Also open:** whether a candidate left untried because the walk stopped
contributes at all. §Understudy counts benched candidates beyond those tried; a
transient `429` ends a walk with later targets never called, and nothing says
whether those are the request's candidates. It decides what the comparison ranges
over.
