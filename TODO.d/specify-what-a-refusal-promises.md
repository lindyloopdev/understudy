# Specify what a refusal promises beyond the request that hit it

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failover walk,
the per-target health a refusal writes, and the Retry-After ladder's terminal
rung, which an all-refusing candidate list reaches.

A refused target demotes and the request replays, and what that replay costs the
caller is specified. What the demotion buys every later caller is not.

- **Should not dial a target again once it has refused the account.** Demotion is
  what stops understudy paying for a refusal it already knows about. A second
  request a second later serves from the healthy backend with the refused one
  never called — its `Excluded` is empty, because the walk skips it rather than
  abandoning it. Its own case, not a step appended to the failover one: that case
  promises this request succeeds, this one promises later requests stay cheap.

- **Should show a refusal was the whole list's verdict, not one target's.** Every
  candidate refusing answers the same `400` `upstream_refused` that a single
  refusing target does, so the status cannot distinguish them. The walk appears
  only on `Excluded`, with each candidate called and refused. Without that, the
  exhaustion path is covered by a case that never walked.
