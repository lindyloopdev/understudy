# Split the upstream limiter's starting allowance from its hard ceiling

**Tag:** understudy / ratelimit / bug

**Design:** [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy),
[§Shared understudy daemon](../UNDERSTUDY.md#shared-daemon),
[§Upstream-identity canonicalization](../UNDERSTUDY.md#upstream-identity-canonicalization).

`newUpstreamLimiter` sets `limit` and `ceiling` to the same constant
(`defaultMaxConcurrentPerUpstream`, 20), and `grow()` only raises `limit` while
`limit < ceiling`. One number therefore serves as both the **starting allowance**
and the **hard maximum**, so the AIMD controller can only ratchet *down* from the
initial guess: `throttle()` seeds from observed in-flight and `shrink()` halves,
but nothing can ever climb above the guess. A too-low value is permanent and
self-confirming.

Split the two: a modest starting allowance (safe cold start) and a separate
ceiling sized as a **safety bound** — bounding in-flight bodies
(`maxRequestBodyBytes`, 32MB each), sockets, and FDs — rather than as a tuned
throughput figure. Then `grow()`'s additive increase can discover real capacity
upward, and the first signal-less 429 still seeds the true limit downward.

Sizing must cover **peak concurrent runs**, not one run: the shared daemon
consolidates every run's traffic into one limiter per account, so N concurrent
`lindy review`s (23 reviewers each) contend for the same slots.

## Constraints

- Keep the ceiling **finite**. The shrink valve fires only on *signal-less* 429s
  (`classifyLimit` → `signalless` → `throttle()`); a 429 carrying `Retry-After`
  classifies as `sustainedRate` and routes to demotion instead, and an upstream
  whose backpressure appears purely as latency never trips it at all. An
  effectively-unbounded ceiling would rely on a brake that may not engage.
- Raising the per-account ceiling removes the *accidental* global bound that 20
  currently provides on the daemon process as a whole. Companion concerns, each
  deferred behind its own trigger rather than built alongside this:
  - **Process-level cap** — add when the per-account ceiling exceeds ~256, or when
    one process serves many tenants. Below that the per-account caps still bound
    the process: observed request bodies are 14–25KB, far under
    `maxRequestBodyBytes`, so the memory case does not carry on its own.
  - **Fairness in `acquire()`** — `wake()` broadcasts and waiters race, so no
    bounded wait is guaranteed. Not supported by evidence yet: the observed
    0.4–177s wait spread is explained by queue *depth* alone (~136 waiters at 20
    slots × ~26s service), not by unfair racing. Revisit only if slot-waits persist
    after this fix. A buffered-channel semaphore would give FIFO for free but
    cannot shrink at runtime — which is why the broadcast exists — so FIFO plus a
    dynamic cap needs a real waiter queue.
  - **Shared-key cold start** — a modest starting allowance is wrong when many
    tenants share one account. Only bites when tenants deliberately share a key;
    settle alongside key custody / multi-tenant work.

## Verify before treating congestion as real

Do **not** add congestion-triggered failover (spilling to alternate targets when
slots are saturated) on the strength of today's queueing. Today's congestion is
imposed by this ratchet, so routing around it would spend money on paid alternates
to work around our own defect — while relieving the queueing enough to mask the
defect.

Once the split lands, re-run the load that produced the evidence below and
distinguish the two causes, which the request log already separates:

- **Self-imposed** — `waiting for an upstream slot (in-flight N)` entries, carrying
  no `upstream_status` because the request never reached the provider.
- **Real** — upstream 429s, or latency degrading while in-flight sits *below* the
  cap.

Only the second justifies failing over on congestion. The tempting observation is
idle alternates: `review-standard`'s `deepseek` and `z-ai` targets took zero traffic
all day while requests waited up to 177s on the primary, because `pickTarget` runs
once per request and slot pressure is deliberately not a failover signal. Changing
that is a separate decision this evidence does not yet support.

## Evidence (2026-07-23)

Six concurrent review runs (~138 concurrent beats) against one upstream saturated
the 20 slots: 47 `waiting for an upstream slot` events, request median 8.3s → 74.2s
and p90 20.4s → 177.1s as concurrency climbed, and beats hitting the 15-minute
deadline. Solo runs never reach the cap and show no tail. Across 943 requests to
`opencode-go` there were zero 429s, so there is no evidence the upstream's own
limit is anywhere near 20.
