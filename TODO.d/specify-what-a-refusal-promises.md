# Specify what a refusal promises beyond the request that hit it

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failover walk,
the per-target health a refusal writes, and the Retry-After ladder's terminal
rung, which an all-refusing candidate list reaches.

A refused target demotes and the request replays, but only the replay half is
specified: the 403 case asserts that this caller gets an answer, and nothing
asserts what the refusal buys every later caller.

- **Should not dial a target again once it has refused the account.** Demotion is
  what stops understudy paying for a refusal it already knows about. A second
  request a second later serves from the healthy backend with the refused one
  never called — its `Excluded` is empty, because the walk skips it rather than
  abandoning it. Its own case, not a step appended to the failover one: that case
  promises this request succeeds, this one promises later requests stay cheap.

- **Should surface the refusal once every candidate has refused.** The walk now
  spends the whole list before answering, which the single-candidate case cannot
  show. The client receives 403 — but typed `server_error`, which a refusal is
  not. Pin the status, and if writing it confirms the type is wrong, leave the
  correct type as a TODO in the test rather than changing the envelope alongside
  a new test.
