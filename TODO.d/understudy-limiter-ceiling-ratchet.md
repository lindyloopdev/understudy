# Let the per-account limiter float; bound the process by an FD budget

**Tag:** understudy / ratelimit / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy),
[§Shared understudy daemon](../DESIGN.md#shared-daemon),
[§Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization).

`newUpstreamLimiter` sets the per-account `limit` and `ceiling` to one constant, so
AIMD can only ratchet *down* from the cold-start guess — a too-low value is
permanent and self-confirming. The hard backstop now lives in the process-wide,
FD-derived slot budget (`chatCompletions` sheds a `503` when it is exhausted), so
the per-account ceiling no longer needs to be the safety bound.

- **Let the per-account limiter float.** Drop the per-account ceiling so `grow()`
  discovers real capacity upward instead of only shrinking. See the `TODO(...)` at
  the per-account acquire site.

## Deferred behind their own triggers

- **Fairness in `acquire()`** — `wake()` broadcasts and waiters race. Revisit only
  if slot-waits persist after this fix; FIFO plus a runtime-shrinkable cap needs a
  real waiter queue.
- **Live `/proc/self/fd` accounting** — tighter than the static startup budget, but
  Linux-only. Add only if the static budget proves too coarse.
- **Shared-key cold start** — a modest starting allowance is wrong when many tenants
  share one account; settle alongside key custody / multi-tenant work.

## Constraint on follow-on work

Do **not** add congestion-triggered failover on the strength of today's queueing —
it is imposed by this ratchet. After the fix lands, re-run the load and separate
self-imposed waits (`waiting for an upstream slot`, no `upstream_status`) from real
upstream 429s; only the latter would justify failing over on congestion.
