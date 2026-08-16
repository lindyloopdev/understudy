# Health writes need a single owner

**Tag:** understudy / refactor / concurrency

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health,
the Retry-After ladder, and why the failover and terminal thresholds are
*durations* rather than failure counts.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the escalating
schedule that resets to base on success, and the rule that a known `readmitAt`
supersedes that schedule entirely.

The rule that a recording path must sweep before it writes is stated in prose,
not enforced: `s.health` is a plain field any new path can assign to, and each
path repeats the `lock, sweep, key` preamble for itself.

- **Decide who owns the health state.** Extract a type that owns the map and
  sweeps inside its only mutating entry point, so the sweep-before-write rule
  is enforced rather than merely followed by convention. This also splits
  `s.mu`, which today also guards `upstreamLimiters`.

Not a data race: `s.mu` serialises every read and write today. This is about
what a health write means, which no lock answers.

## Not caused by the reference-parsing work

A request naming `<backend>/<model>` directly writes health like any other —
that's deliberate, not a defect the reference-parsing work introduced.
