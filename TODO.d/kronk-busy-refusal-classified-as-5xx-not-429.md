# [BUG] Kronk's busy refusal relays as 503/400, not the 429 it was planned around

**Tag:** understudy / ratelimit / bug / high-priority

**Design:** [DESIGN.md §Concurrency & Rate
Limiting](../DESIGN.md#concurrency-rate-limiting). Cross-repo context: lindy's
[TODO.d/retry-ignores-retry-after.md](https://github.com/lindyloopdev/beat/blob/main/TODO.d/retry-ignores-retry-after.md)
and
[notes/2026-08-16-kronk-503-opencode-visibility.md](https://github.com/lindyloopdev/beat/blob/main/notes/2026-08-16-kronk-503-opencode-visibility.md).

## The gap

`kronkpool.ErrServerBusy` (`503`, `code: "unavailable"`) — a model-swap
refusal because the GPU slot it needs is mid-stream on another model — is
caught by its own dedicated branch (`errors.Is(err, providers.ErrServerBusy)`,
`understudy.go:2276`), separate from `classifyLimit`'s rate-limit
classification (`isRateLimit = status == http.StatusTooManyRequests`,
`understudy.go:1359`). That branch relays the failure as a `503` carrying a
*synthesized* `Retry-After` (`graduatedBackoff`, doubling from 5s), and once
the cumulative observed-busy time crosses `terminalThreshold` (2 min), converts
it to a `400` reject — never a `429`.

lindy's own TODO documents this as a known divergence from intent: *"kronk's
`503` coded `unavailable` relays as a `503` carrying `Retry-After`, **not the
429 this was planned around**, and only turns terminal past understudy's
threshold."* That wording implies `ErrServerBusy` was meant to be routed
through the same 429/rate-limit classification and demotion table
(`DESIGN.md:900-903`) as a real quota rate-limit, rather than through its own
501/503-shaped branch. As implemented, it never reaches that table.

## Why it matters

A busy-refusal and a rate-limit are semantically the same thing to a caller —
"come back later, this isn't broken" — and treating them identically (429 +
the existing demotion/streak machinery) would give Kronk contention the same
handling quota exhaustion already gets, instead of a bespoke synthesized-backoff
path that duplicates logic.

**Open question before assuming "emit 429" alone is the whole fix:** lindy
measured (2026-08-16, real `opencode serve` v1.15.13) that a `503` — with or
without `Retry-After` — is silently swallowed by opencode's own internal
retry and never surfaces as a `session.error`; only 4xx responses (421, 498,
400) were observed crossing intact. Whether opencode's outer retry layer
treats a `429` the same "swallow and retry indefinitely, no cap" way as a
5xx is not yet verified here. If it does, relabeling `ErrServerBusy` from
503 to 429 changes which classification/demotion table it flows through in
understudy but does **not** by itself fix the underlying visibility gap —
lindy still can't see or react to the beat while opencode is retrying quietly.
Verify that before treating this as the complete fix.

## Work

- Route `ErrServerBusy` through `classifyLimit`'s rate-limit path (or extend
  it) instead of the separate busy-target branch, so it's relayed as `429`
  with the observed/synthesized delay, subject to the same demotion table as
  a real rate limit.
- Verify (against real opencode) whether a 429 in this shape is any more
  visible to lindy than the 503 it replaces, before calling the visibility
  problem solved.
