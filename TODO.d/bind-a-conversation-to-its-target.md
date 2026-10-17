# Bind a conversation to its target

**Tag:** understudy / ha / fallback

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Affinity and
admission (staged)" and its *Session identity* dependency: what affinity is, when
it engages, how long it lasts, and how it degrades.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the half-open probe
a bound request would otherwise divert.

## Work

- **Count a target's query overrides in its identity to a conversation.**
  Preferring the last-served target, and telling a 400 from "another target
  served last", compare the whole target, overrides included; health still
  keys on the account and model.
- **Treat two requests in flight on one key as a collision.** A conversation is
  serial, so the later arrival is a different one whatever produced the same key,
  and takes the normal walk. This is what degrades quietly if the caller stops
  putting distinguishing content in the first user turn.
- **Decide whether a bound request may carry a due recovery probe.** Preferring
  the recorded target skips the probe; letting the probe win moves the
  conversation. The first-turn rule keeps them apart until a bound conversation
  outlives a demotion.
- **Confirm on the wire that a compacted request drops the first user turn.**
  The target choice starts over at compaction only because the key changes. Read
  from the shipped opencode bundle 2026-08-14 — compaction cuts at a user-message
  boundary against a `preserve_recent_tokens` budget — but not yet seen on a
  captured body.
- **Relay a `400` to the client when it answers a conversation another target
  served more than a day before.** The failover that reads the record lapses
  with the record, so a history rejection after the record is forgotten again
  looks like the client's.
- **Settle what a caller-initiated model switch means for affinity.** opencode's
  session carries `ModelSwitched` and `AgentSwitched`, so the caller can change
  model without understudy; affinity that fought that would be wrong.
