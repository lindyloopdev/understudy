# Per-account limiter & FD budget: deferred refinements

**Tag:** understudy / ratelimit

**Design:** [DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting),
[§Shared understudy daemon](../DESIGN.md#shared-daemon),
[§Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization).

## Bring the control law up to the design

The limiter grows by one slot per demand-gated success, not by the law
[§Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) states:

- **Split the growth regimes at the last known-good cap.** One slot per success is
  doubling-per-round under saturation, which is right far from the edge and wrong near
  it. Fall back to one slot per round at or above the known-good boundary.

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
