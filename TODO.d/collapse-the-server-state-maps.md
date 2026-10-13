# Collapse the server's duplicate-keyed state maps

**Tag:** understudy / internals

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — *Affinity and
admission* (the conversation binding and its idle release); [DESIGN.md
§Session-Ordered Admission](../DESIGN.md#session-ordered-admission) — a
session's rank.

Two maps hang off `server` under one `mu` with the same key:

| Map | Key | Value |
| --- | --- | --- |
| `affinity` | tenant → conversation key | `lastServed` |
| `sessionAges` | tenant → conversation key | first-seen rank and last-seen time |

One record per conversation would say the same thing: its bound target and its
admission standing — the rank it was first seen at, and when it last took a
turn. (Target health stays apart from the admission queues:
[notes/2026-10-13-health-stays-apart-from-admission-queues.md](../notes/2026-10-13-health-stays-apart-from-admission-queues.md).)

## Why it is not a mechanical merge

The pair keeps two lifetimes, and a single record with a single expiry would
break one of them. **Affinity releases at `affinityIdleTTL` (5m, sized to the
provider's prefix cache) via `evictIdleAffinity`; a session's rank must outlive
it** — kept for the session's life, dropped only on the consumer's end signal or
once the conversation has gone `sessionForgetAfter` (a day) without a turn
([[session-ordered-admission]]). The two are measured from different instants:
affinity expires from when the conversation was last served, the rank from when
it last took a turn, and the rank itself is the *first* sighting, which never
moves while the session lives. Merging therefore needs per-field expiry: drop
the bound target at the idle TTL, and keep the rank until the last-seen time
falls that far behind.

## Work

- Merge the pair into one record, keeping `mu`'s single-lock discipline.
- Give each field its own expiry rule, per above.
- Strict refactor: no behavior change, no test diff.
