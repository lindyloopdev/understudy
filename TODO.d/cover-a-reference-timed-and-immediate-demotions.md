# Cover the demotions a directly-named reference writes

**Tag:** understudy / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the Retry-After
ladder's three demotion shapes and the rule that health belongs to the endpoint,
not to the route that reached it.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — an advertised
`readmitAt` superseding the schedule, which the timed bench is what creates.

A request naming `<backend>/<model>` directly writes the shared health entry
through all three paths, but only the streak-accruing one (`recordFailure`, from a
502/503) is driven that way by a test.

- **A 429 carrying a Retry-After past the demotion threshold benches the shared
  account.** Name `a/ma` directly, answer with that 429, then have a logical model
  on the same account dial elsewhere before the advertised time elapses. This is
  the shape a streak cannot stand in for: `recordRateLimited` writes `readmitAt`,
  which supersedes the schedule rather than accruing against a threshold.
- **A refused credential demotes at once.** Name `a/ma` directly, answer 401 or
  402, and show the very next logical-model request routing around the account
  with no interval elapsing.

Both belong in `TestChatCompletionsFailoverRouting`, whose `step.model` already
lets a case name a reference directly.
