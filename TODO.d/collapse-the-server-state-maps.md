# Collapse the server's duplicate-keyed state maps

**Tag:** understudy / internals

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and
admission* (the conversation binding and its idle release) and *Stalls* (the
per-target health the walk reads); [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — the queue's key and a
session's rank.

Six maps hang off `server` under one `mu`, in three key spaces, each of which
holds two maps:

| Map | Key | Value |
| --- | --- | --- |
| `health` | `healthKey` (canonical url + key + model) | `targetHealth` |
| `admission` | `healthKey` — the same key | `*targetQueue` |
| `affinity` | tenant → conversation key | `lastServed` |
| `sessionAges` | tenant → conversation key — the same key | first-seen rank and last-seen time |
| `busyRefusals` | `canonicalUpstreamKey` (url + key) | `*busyRefusal` |
| `upstreamLimiters` | `canonicalUpstreamKey` — the same key | `*upstreamLimiter` |

Three records would say the same thing: one per target holding its health and
its queue, one per conversation holding its bound target and its admission
standing — the rank it was first seen at, and when it last took a turn — and
one per upstream account holding its limiter and its standing busy refusal.

## Why it is not a mechanical merge

Each pair keeps two lifetimes, and a single record with a single expiry would
break one of them:

- **Affinity releases at `affinityIdleTTL` (5m, sized to the provider's prefix
  cache) via `evictIdleAffinity`; a session's rank must outlive it** — kept for
  the session's life, dropped only on the consumer's end signal or once the
  conversation has gone `sessionForgetAfter` (a day) without a turn
  ([[session-ordered-admission]]). The two are measured from different
  instants: affinity expires from when the conversation was last served, the
  rank from when it last took a turn, and the rank itself is the *first*
  sighting, which never moves while the session lives. Merging therefore needs
  per-field expiry: drop the bound target at the idle TTL, and keep the rank
  until the last-seen time falls that far behind.
- **`evictStaleHealth` sweeps health, but a `targetQueue` can hold goroutines
  parked waiting to be admitted.** Evicting a queue with waiters splits it: a
  new arrival gets a fresh queue and is sent ahead of them. So the sweep must
  skip a queue that is not empty (or the queue must hold the record alive
  itself).

## Work

- Merge each pair into one record, keeping `mu`'s single-lock discipline.
- Give each field its own expiry rule, per above.
- Strict refactor: no behavior change, no test diff.
  - Tests: "should hold a new request at a target behind a request that has
    waited there longer than the target's health record is kept" pins the
    queue's survival.
