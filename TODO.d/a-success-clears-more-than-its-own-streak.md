# A success clears more than its own streak

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health,
the Retry-After ladder, and why the failover and terminal thresholds are
*durations* rather than failure counts.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the escalating
schedule that resets to base on success, and the rule that a known `readmitAt`
supersedes that schedule entirely.

`clearFailure` deletes the whole health entry on any success. Ending the streak
that way is deliberate (§Understudy, "A success clears the streak"), and dropping
`lastTouch` is right — it exists for the sweep. The entry also holds `readmitAt`,
which is not a measurement understudy took but an instruction the provider gave,
and a success is not the provider withdrawing it. Health is shared per
`(url + key + model)` (§Understudy, "Health belongs to the endpoint"), so the
success doing the deleting need not be the request, the route, or even the client
that learned what it erases.

- **Keep an upstream's `readmitAt` across a success.** An upstream answers `429
  Retry-After: 300`; a concurrent request to the same account and model returns
  200; the entry — bench included — is gone, and understudy resumes sending
  traffic the provider explicitly asked it to hold. The design already settles
  which wins: the upstream's time "supersedes the schedule entirely … there is
  nothing to discover." A success is not the provider withdrawing that
  instruction. Narrow the clear so it ends the streak and leaves `readmitAt`
  standing until it elapses. Drive it from two concurrent requests to one
  account, not from a sequence — the ordering is the behavior.

- **Decide who owns the health state while changing what a write means.** The rule
  that a recording path must sweep before it writes is stated in prose, not
  enforced: `s.health` is a plain field any new path can assign to, and each path
  repeats the `lock, sweep, key` preamble for itself.
  Extracting a type that owns the map and sweeps inside its only mutating entry
  point is the version that enforces it, and it splits `s.mu`, which today also
  guards `upstreamLimiters`. Doing that here rather than separately means one pass
  over health ownership instead of two, since narrowing `clearFailure` rewrites
  these same functions.

Neither is a data race: `s.mu` serialises every read and write. Both are about
what a health write means, which no lock answers.

## Not caused by the reference-parsing work

A request naming `<backend>/<model>` directly writes health like any other, which
raises how often both effects occur and lets such a caller lift a bench others are
respecting. Two concurrent logical-model requests could always do this; reviewers
reading it as new behavior are reading the widened writer set, not a new defect.
