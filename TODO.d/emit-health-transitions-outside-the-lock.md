# Emit health transitions outside the health lock

**Tag:** understudy / concurrency

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the per-target health
the failover walk consults, and "understudy owns its telemetry record; what a
consumer does with it is the consumer's", which is what makes the logger a
consumer's code running inside understudy's critical section.

The two backend up/down transitions are written to `s.logger` while `s.mu` is held:
`noteBackendDown`, called from `pickTarget`'s walk, and `clearFailure`. The logger is
supplied by the consumer through `WithLogger`, so a handler doing synchronous I/O —
a file, a socket, a collector — blocks every other request's `pickTarget` for as long
as the write takes, because they all serialize on that one mutex.

Not a deadlock: a handler cannot reach `s.mu`, having no reference to the server.
A latency coupling between the operator's logging setup and request routing, which
shows up only under a slow sink.

- Collect the transitions the critical section decides on and emit them after
  `Unlock`, the shape `pickTarget` already uses for skip reasons: it returns them and
  the walk records.
- The transitions are edge-triggered off `downLogged`, which is health state written
  under the lock, so the decision has to stay inside — only the emission moves.
- Unmeasured. Worth a benchmark that holds a slow handler against concurrent picks
  before deciding how much it matters.
