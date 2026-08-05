# Weigh every candidate's contribution, not the first throttle

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs
out of candidates answers for the request, not for its last target", the
contribution table beneath it (what each disposition offers), and "the verdict is
the soonest contribution, answered in the shape of the candidate that made it".

The walk keeps one candidate's error and prefers it only over a refusal, so two
measured cases answer worse than the design requires:

- **First-wins, not soonest.** `[a: 429/30m, b: 429/60s, c: 401]` answers `400`
  `upstream_rate_limited` with `retry_after_ms: 1800000` — `a` is captured and
  `b` discarded, and 30m past the passthrough ceiling turns the answer terminal.
  The client is told to stop for half an hour while `b` would serve in a minute.
  Each contribution has to be weighed against the incumbent; the walk keeps the
  first it sees.
- **Only a refusal yields.** `[a: 429/60s, b: 401, c: 501]` answers a bare `502`,
  discarding `a`'s 60s because the walk ended on a `501` rather than a refusal.
  Every zero-contribution ending should yield — a never-retryable `5xx`, an
  unusable target, a plain `5xx` that ends the walk where it falls.

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
