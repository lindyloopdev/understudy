# Per-account limiter & FD budget: deferred refinements

**Tag:** understudy / ratelimit

**Design:** [DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting),
[§Shared understudy daemon](../DESIGN.md#shared-daemon),
[§Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization).

## Bring the control law up to the design

The limiter grows and shrinks, but not by the law
[§Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) states. Two
gaps, each independently landable:

- **Split the growth regimes at the last known-good cap.** One slot per success is
  doubling-per-round under saturation, which is right far from the edge and wrong near
  it. Track the known-good boundary and fall back to one slot per round at or above it.
- **Make the decrease a measurement.** `throttle()`'s `seeded` latch never resets, so
  every 429 after the first halves the cap for the rest of the process's lifetime, and
  per-upstream state outlives tenants by design. Record the cap a saturated rejection
  measures as the known-good boundary, and reserve halving for a repeat rejection at or
  below that boundary.

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
