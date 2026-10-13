# Expose operator metrics

**Tag:** understudy / observability

**Design:** none yet. DESIGN.md has no metrics surface; decide it there
first.

An operator running the daemon for weeks cannot see how much state it holds:
how many conversations it tracks, how many affinity records, how deep each
admission queue is. So a leak in any of them shows only as memory growth, and
a fix cannot be confirmed. Understudy is embedded as a library, so its metrics
register with the embedder's registry (e.g. an option taking a
`prometheus.Registerer`), never a global one; the embedder decides whether and
where to serve them.

## Work

- **Decide the metrics surface in DESIGN.md**: how the embedder supplies the
  registry, and what is exposed.
- **A gauge of the conversations the daemon tracks** (`conversations` with an
  admission standing).
  - Tests: a conversation quiet past `sessionForgetAfter` is no longer counted
    once another conversation takes a turn. This is the test the eviction
    (`evictForgottenSessions`) ships without.
- **Other candidates**: affinity records, health entries, admission queue
  depth.
