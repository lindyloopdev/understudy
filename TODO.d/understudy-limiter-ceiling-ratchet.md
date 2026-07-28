# Per-account limiter & FD budget: deferred refinements

**Tag:** understudy / ratelimit

**Design:** [DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting),
[§Shared understudy daemon](../DESIGN.md#shared-daemon),
[§Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization).

## Deferred refinements

Each behind its own trigger:

- **Fairness in `acquire()`** — `wake()` broadcasts and waiters race. Revisit only if
  slot-waits persist; FIFO plus a runtime-shrinkable cap needs a real waiter queue.
- **Live `/proc/self/fd` accounting** — tighter than the static startup budget, but
  Linux-only. Add only if the static budget proves too coarse.
- **Shared-key cold start** — a modest starting allowance is wrong when many tenants
  share one account; settle alongside key custody / multi-tenant work.

## Constraint on follow-on work

Do **not** add congestion-triggered failover on the strength of slot queueing — it is
self-imposed admission control, not upstream backpressure. Only a real upstream 429
(vs. a `waiting for an upstream slot` wait with no `upstream_status`) would justify
failing over on congestion.
