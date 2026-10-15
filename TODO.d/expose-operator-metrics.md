# Expose operator metrics

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#operator-counts) — *Operator
counts are a snapshot the embedder reads*.

An operator running the daemon for weeks cannot see how much state it holds,
so a leak shows only as memory growth, and a fix cannot be confirmed.

## Work

- **The count of conversations with an admission standing**, the snapshot's
  first count.
  - Tests: a conversation quiet past `sessionForgetAfter` is no longer counted
    once another conversation takes a turn. This is the test the eviction
    (`evictForgottenSessions`) ships without.
- **The snapshot's other counts**: conversations with an affinity record,
  targets with a health entry, and requests waiting in admission queues.
