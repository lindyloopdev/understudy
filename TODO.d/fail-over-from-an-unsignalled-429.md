# Fail over from a 429 the client will never wait out

**Tag:** understudy / ratelimit / fallback

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the Retry-After
ladder and the failover walk the replay branch reads.
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting)
— the wait-vs-failover policy this entry disputes for the unsignalled case.

`signalless` and `transientRate` both answer the client a `429` and decline to
demote or fail over, on the premise that the client waits the
`Retry-After` out. Measured in the consumer, nothing does:

- opencode's agent turn runs at `maxRetries: input.retries ?? 0`
  (`session/llm.ts:309` at v1.15.13); the agent path passes no `retries`, so the
  AI SDK makes exactly one attempt. Only title generation sets `retries: 2`.
  Confirmed 2026-08-14 in the shipped bundle, where the SDK's own helper defaults
  to 2 only when the value is null, so an explicit `0` never reaches its delay
  logic — [notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md](../notes/2026-08-12-opencode-retries-any-5xx-and-honors-retry-after.md).
- lindy's own `beat.RetryTransient` returns immediately for any status in
  `[400,500)`, so 429 is terminal above opencode too.

So the backoff is written to a reader that does not exist, and the
request dies on a target understudy has judged healthy and kept in service.

Measured 2026-08-11 in lindy: one `review-examine` run lost 25 requests this way,
each a dead reviewer, while untried targets remained in the logical model. The
responses carried `retry-after: 60` — `synthesizedRateLimitRetryAfter` exactly —
so the upstream sent nothing and the condition was `signalless`, the
degenerate z.ai case [[understudy-ratelimit-signal-classifier]] describes.
`transientRate` shares the premise and the same fate, but is not what production
hit.

## Work

- Widen the within-request replay branch (`sustainedRate || isAccessRefused` in
  `chatCompletions`) to replay a `signalless` 429 onto an untried target. For an
  unsignalled limit — most likely a concurrency ceiling — another target is the
  answer, and the walk already knows which are untried.
- Decide `transientRate` on the same question rather than by analogy: a genuinely
  brief throttle it named may still be worth waiting *if* something waits. As
  long as nothing does, surfacing it is a lost request too.
- Keep demotion out of it. Neither condition judges the target unhealthy, and
  the point is to route this request around a momentary limit, not to bench a
  target for the ones behind it.
- Decide what the walk answers once replay has nowhere left to go. It cannot
  simply relay the last 429 to a client that will not act on it — that is the
  same dead end one target further along, and it meets the benched-list question
  [[honor-an-upstream-backoff-with-nothing-left]] is holding.

[[fail-over-in-place-from-a-demoted-target]] widens the same branch for a 5xx
past the failover threshold; whichever lands first should leave the condition
shaped for the other.
