# Attribute each target demotion to the request that caused it

**Tag:** understudy / observability / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

A `backend down` transition should always have a corresponding logged request
that caused it. Observed anomaly at the start of a review run: google was
demoted (`backend down google`) with **no logged google request** preceding it
— the only earlier entry was `POST /session`. A demotion requires a prior
failing request (`recordFailure` / `recordImmediateFailure` /
`recordRateLimited` from the demote switch), so either that request is not
being recorded with its backend (a logging gap), or there is a demotion path
that fires without a real request (spurious).

Make every demotion attributable: ensure the demoting request is logged with
its backend/status, and if a demotion can occur without one, surface that path.
