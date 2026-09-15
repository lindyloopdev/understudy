# Decide whether the admission queue replaces the learned concurrency cap

**Tag:** understudy / ratelimit / admission

**Design:** [DESIGN.md §Concurrency & Rate
Limiting](../DESIGN.md#concurrency-rate-limiting) — the learned cap and the bare
429 as its measurement; [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — the 429 rule that re-queues
a request while its key has a running stream.

Both answer "more requests than the upstream will take". The cap learns the
limit and avoids sending past it; the queue ignores the limit, re-queuing a
rejected request until a running one finishes. Once
[[session-ordered-admission]] lands, the queue may make the cap redundant.

## Work

- Gather the evidence first:
  - Does any provider penalize repeated 429s (escalating backoff, temporary
    key lockout)? The cap avoids provoking rejections; the queue absorbs about
    one per completion while contended.
  - Are concurrency limits per account or per model? The cap is per account;
    the queue is per model, so an account-wide limit would reject each model's
    queue separately unless linked the way a busy refusal links a server's
    models. Related: [[key-the-concurrency-limiter-on-model]].
  - The cap is always on; the queue forms only under contention, so a first
    burst to a remote goes out in full.
- Then decide: retire the cap (a behavior removal — delete it and its tests
  together), keep both, or fold the cap's account scope into the queue.
