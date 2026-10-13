# Target health stays apart from the admission queues

Point-in-time decision (2026-10-13). [[collapse-the-server-state-maps]] had
proposed merging `health` and `admission`, which share the per-target
`healthKey`, into one per-target record. That merge was dropped; the
conversation pair (`affinity` and `sessionAges`) stays in that item.

## Why

The rule is to choose the shape that leaves the code easiest to change next.
The next known change to this state is [[health-writes-need-a-single-owner]]: a
type that owns the health map and sweeps inside its only mutating entry point,
with its own lock out from under `s.mu`. Kept apart, that type holds only health
and its sweep. Merged, it would also own the queues, and its sweep would have
to clear the health field while keeping any record whose queue exists.

The two lifetimes do not match. A health entry is swept once untouched for
`healthTTL`; a queue is never dropped, because dropping one splits it — a new
arrival gets a fresh queue and is sent ahead of the requests still waiting in
the old one. "should hold a new request at a target behind a request that has
waited there longer than the target's health record is kept" pins that.

The merge's gain was one lookup per target and a home for future per-target
state. Only `pickTarget` and `anotherTargetTakingNewSessions` read both maps
today, and no planned
per-target field needs the shared home.

## Revisit when

Per-target state beyond health and the queue is planned, or the single-owner
type is built and a per-target record would then sit beside it without taking
its sweep.
