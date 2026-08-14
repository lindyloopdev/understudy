# Fail over from a throttle the client will never wait out

**Tag:** understudy / ratelimit / fallback

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the Retry-After
ladder and the failover walk the replay branch reads.
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting)
— a rate limit read as a capacity signal rather than a health verdict, which is
why routing around one leaves the target's health alone.

`transientRate` answers the client a `429` and declines to demote or fail over, on
the premise that the client waits the `Retry-After` out. Measured in the consumer,
nothing does:

- opencode's agent turn runs at `maxRetries: input.retries ?? 0`
  (`session/llm.ts:309` at v1.15.13); the agent path passes no `retries`, so the
  AI SDK makes exactly one attempt. Only title generation sets `retries: 2`.
  Confirmed 2026-08-14 in the shipped bundle — [notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md](../notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md).
- lindy's own `beat.RetryTransient` returns immediately for any status in
  `[400,500)`, so 429 is terminal above opencode too.

So the backoff is written to a reader that does not exist and the request dies on a
target understudy kept in service. Under the sibling condition
[[understudy-ratelimit-signal-classifier]] describes, that cost one `review-examine`
run 25 requests on 2026-08-11, untried targets still in the model.

## Work

- Decide `transientRate` on the question its sibling settled, rather than by
  analogy: a genuinely brief throttle the upstream named may still be worth waiting
  *if* something waits. As long as nothing does, surfacing it is a lost request too.
- Keep demotion out of it. The condition does not judge the target unhealthy, and
  the point is to route this request around a momentary limit, not to bench a
  target for the ones behind it.
- Decide what the walk answers once replay has nowhere left to go. It cannot
  simply relay the last 429 to a client that will not act on it — that is the
  same dead end one target further along, and it meets the benched-list question
  [[honor-an-upstream-backoff-with-nothing-left]] is holding.

[[fail-over-in-place-from-a-demoted-target]] widens the same branch for a 5xx
past the failover threshold; whichever lands first should leave the condition
shaped for the other.
