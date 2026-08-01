# A success clears more than its own streak

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health,
the Retry-After ladder, and why the failover and terminal thresholds are
*durations* rather than failure counts.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the escalating
schedule that resets to base on success, and the rule that a known `readmitAt`
supersedes that schedule entirely.

`clearFailure` deletes the whole health entry on any success. The entry holds five
things — `failingSince`, `lastProbe`, `readmitAt`, `downLogged`, and `lastTouch` —
and a success is evidence about only the first. Deleting is right for `lastTouch`,
which exists for the eviction sweep and has nothing to say once the entry is gone;
the middle three it erases on the strength of one request. Health is shared per
`(url + key + model)` (§Understudy, "Health belongs to the endpoint"), so the
success doing the deleting need not be the request, the route, or even the client
that learned any of what it erases.

- **Keep an advertised `readmitAt` across a success.** An upstream answers `429
  Retry-After: 300`; a concurrent request to the same account and model returns
  200; the entry — bench included — is gone, and understudy resumes sending
  traffic the provider explicitly asked it to hold. The design already settles
  which wins: an advertised time "supersedes the schedule entirely … there is
  nothing to discover." A success is not the provider withdrawing that
  instruction. Narrow the clear so it ends the streak and leaves `readmitAt`
  standing until it elapses. Drive it from two concurrent requests to one
  account, not from a sequence — the ordering is the behavior.

- **Settle whether one success should clear a streak many requests accrued.**
  `failingSince` is set only when no entry exists and any success removes the
  entry, so crossing the 15s failover threshold requires 15s with *zero*
  successes on that account. A target failing 30% of the time under load never
  demotes; the same target at the same rate serving one client might. §Understudy
  makes the threshold "a *duration* (≈15s), not a failure count" precisely to be
  volume-independent — the reasoning is spelled out in
  [[understudy-adaptive-coordinated-backoff]] — and the clear reintroduces the
  volume dependence behind the threshold's back. It is not obviously wrong: a 70%-healthy
  target may well beat no target, and reset-on-success is what lets a recovered
  host snap back ([[understudy-adaptive-coordinated-backoff]]). But the design
  does not say so, and the ladder's rungs read as though it does not hold. Decide
  in DESIGN.md what a success is evidence *of* — §Concurrency & Rate Limiting
  already refuses to move the limiter on a success that met no contention — then
  build to whatever that settles.

Neither is a data race: `s.mu` serialises every read and write. Both are about
what a success means, which no lock answers.

## Not caused by the reference-parsing work

A request naming `<backend>/<model>` directly writes health like any other, which
raises how often both effects occur and lets such a caller lift a bench others are
respecting. Two concurrent logical-model requests could always do this; reviewers
reading it as new behavior are reading the widened writer set, not a new defect.
