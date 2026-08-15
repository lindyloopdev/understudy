# Pin what the walk answers when a transient 429 exhausts every target

**Tag:** understudy / ratelimit / fallback / test

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the Retry-After
ladder's "list exhausted → surface / reject" rung, which this case exercises with
`transientRate` as the exhausting condition.

`transientRate` now joins the within-request replay (`chatCompletions`' replay
condition at understudy.go:2319 was widened to include it), but every case
exercising that only covers the walk finding a healthy fallback. Nothing covers the walk running out of candidates while the last
failure is itself a `transientRate` 429 — a single-target config, or every target
transiently rate-limited.

Traced against the terminal epilogue (understudy.go:2365-2378): the `throttled`
substitution only fires when the *last* failure is `isAccessRefused` or `501`
(line 2371-2372), neither of which a `transientRate` 429 is, so the trailing 429 is
relayed as-is through `withSynthesizedBackoff`/`terminalFailure` — the same
"list exhausted" rung already proven for `sustainedRate` by "should surface the
429 when every target is rate-limited past the threshold" (understudy_test.go:3994).
The existing mechanism is very likely already correct for this case; it is only
unverified.

- Add "should relay a transientRate 429 to the client when the walk exhausts every
  target", mirroring the sustainedRate case at understudy_test.go:3994 but with a
  Retry-After below `rateLimitDemotionThreshold`, and no healthy target left to
  route to.
- No implementation change expected — if the case fails, that is itself the
  finding.
