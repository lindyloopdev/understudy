# [BUG] Verify a 429 actually fixes kronk busy-refusal visibility for opencode

**Tag:** understudy / ratelimit / bug / high-priority

**Design:** [DESIGN.md §Concurrency & Rate
Limiting](../DESIGN.md#concurrency-rate-limiting). Cross-repo context: lindy's
[TODO.d/retry-ignores-retry-after.md](https://github.com/lindyloopdev/beat/blob/main/TODO.d/retry-ignores-retry-after.md)
and
[notes/2026-08-16-kronk-503-opencode-visibility.md](https://github.com/lindyloopdev/beat/blob/main/notes/2026-08-16-kronk-503-opencode-visibility.md).

`ErrServerBusy` now normalizes to a 429 with a synthesized, jittered
Retry-After and flows through `classifyLimit`'s ordinary rate-limit path —
same demotion, backoff, replay, and terminal-reject machinery a real
sustained rate limit gets, no bespoke branch of its own.

**Open, and the only work left:** lindy measured (2026-08-16, real `opencode
serve` v1.15.13) that a `503` — with or without `Retry-After` — is silently
swallowed by opencode's own internal retry and never surfaces as a
`session.error`; only 4xx responses (421, 498, 400) were observed crossing
intact. Whether opencode's outer retry layer treats a `429` the same
"swallow and retry indefinitely, no cap" way as a 5xx has not been verified.
Verify against real opencode whether the 429 this now sends is actually more
visible to lindy than the 503 it replaced — if not, the underlying
visibility gap is still open regardless of this reclassification.
