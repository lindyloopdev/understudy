# Bind a conversation to its target

**Tag:** understudy / ha / fallback

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Affinity and
admission (staged)" and its *Session identity* dependency: what affinity is, when
it engages, when it releases, and how it degrades.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the half-open probe
a bound request would otherwise divert.

## Work

- **Treat two requests in flight on one key as a collision.** A conversation is
  serial, so the later arrival is a different one whatever produced the same key,
  and takes the normal walk. This is what degrades quietly if the caller stops
  putting distinguishing content in the first user turn.
- **Decide whether a bound request may carry a due recovery probe.** Preferring
  the recorded target skips the probe; letting the probe win moves the
  conversation. The first-turn rule keeps them apart until a bound conversation
  outlives a demotion.
- **Confirm on the wire that a compacted request drops the first user turn.**
  Affinity releases at compaction only because the key changes. Read from the
  shipped opencode bundle 2026-08-14 — compaction cuts at a user-message boundary
  against a `preserve_recent_tokens` budget — but not yet seen on a captured body.
- **Drop a tenant's affinity when it goes**, rather than leaving it to the TTL.
  No registry exists here yet, so it lands with
  [[understudy-shared-daemon-subserver]].
- **Settle what a caller-initiated model switch means for affinity.** opencode's
  session carries `ModelSwitched` and `AgentSwitched`, so the caller can change
  model without understudy; affinity that fought that would be wrong.

## Constraint

The idle sweep that reclaims affinity cannot be pinned by a test — no consumer
reads the map's size, the same absence the health map has — so it rests on review.
Keep it visible in the code.
