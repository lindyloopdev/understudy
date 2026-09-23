# Collapse the server's duplicate-keyed state maps

**Tag:** understudy / internals

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and
admission* (the conversation binding and its idle release) and *Stalls* (the
per-target health the walk reads); [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — the queue's key and a
session's rank.

Five maps hang off `server` under one `mu`, in three key spaces — two of which
hold two maps each:

| Map | Key | Value |
| --- | --- | --- |
| `health` | `healthKey` (canonical url + key + model) | `targetHealth` |
| `admission` | `healthKey` — the same key | `*targetQueue` |
| `affinity` | tenant → conversation key | `lastServed` |
| `sessionAges` | tenant → conversation key — the same key | first-seen time |
| `upstreamLimiters` | `canonicalUpstreamKey` (url + key) | `*upstreamLimiter` |

Two records would say the same thing: one per target holding its health and its
queue, one per conversation holding its bound target and its first sighting.

## Why it is not a mechanical merge

Each pair keeps two lifetimes, and a single record with a single expiry would
break one of them:

- **Affinity releases at `affinityIdleTTL` (5m, sized to the provider's prefix
  cache) via `evictIdleAffinity`; a session's rank must outlive it** — kept for
  the session's life, dropped only on the consumer's end signal or the
  day-long backstop ([[session-ordered-admission]]). Merging needs per-field
  expiry: drop the bound target at the idle TTL, keep the first-seen time to the
  backstop.
- **`evictStaleHealth` sweeps health, but a `targetQueue` can hold goroutines
  parked on a release.** Evicting a queue with waiters strands them, so the
  sweep must skip a queue that is not empty (or the queue must hold the record
  alive itself).

## Work

- Merge each pair into one record, keeping `mu`'s single-lock discipline.
- Give each field its own expiry rule, per above, and pin both in tests: an
  idle conversation loses its affinity but keeps its rank; a target with a
  non-empty queue is not swept.
- Strict refactor: no behavior change, no test diff beyond the two expiry
  pins, which are behavior the existing sweeps already own.
