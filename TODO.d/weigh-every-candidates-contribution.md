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
- An **earlier candidate's synthesized interval** — the wait understudy hands a
  replayed-past `429` that sent no delay of its   own — is not weighed: only
  a delay the incumbent's upstream sent displaces the failure that ended the
  walk. Measured: an earlier bare `429` against a later target's Retry-After of
  120 is answered with 120.
- A **stall** offers the synthesized stall backoff.
- A walk ending on a target **unusable as configured** still discards an earlier
  throttle. Reachability unverified: `pickTarget` skips such targets rather than
  ending a walk with their error.

So the rest of the table comes next, together — the last failure's synthesized
interval is now weighed, which was what the bench row waited on. Building the
bench row alone would honor the promise in one corner and break it in the common
case, which is worse than breaking it everywhere.

**Also open:** whether a candidate left untried because the walk stopped
contributes at all. §Understudy counts benched candidates beyond those tried; a
transient `429` ends a walk with later targets never called, and nothing says
whether those are the request's candidates. It decides what the comparison ranges
over.
