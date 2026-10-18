# Expose operator metrics

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#operator-counts) — *Operator
counts are a snapshot the embedder reads*.

An operator running the daemon for weeks cannot see how much state it holds,
so a leak shows only as memory growth, and a fix cannot be confirmed.

## Work

- **The snapshot's other counts**: conversations with an affinity record,
  targets with a health entry, and requests waiting in admission queues.
