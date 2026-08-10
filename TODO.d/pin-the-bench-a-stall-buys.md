# Pin the bench a stall buys

**Tag:** understudy / test

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — a target
held to a known re-admission moment is routed around until it, then re-admitted as a
half-open probe. [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure
table's "stalled before its header | the synthesized stall backoff".

A pre-header stall benches its target for `synthesizedStallBackoff`, a window
understudy invents because the upstream named none. Neither end of that window is
asserted: the stall cases watch the next request a second later, and nothing observes
the target still being routed around at 29 seconds or called again at 31.

Measured: cutting the constant from 30s to 5s breaks no test.

The behavior to pin: **should route around a stalled target for the bench it
synthesized, and call it again once that bench elapses.** One case covers both ends —
a request inside the window that serves from the sibling, and one past it that
reaches the stalled target.

Note [[understudy-adaptive-coordinated-backoff]] would replace the fixed 30s with a
per-endpoint interval. The case should assert against the interval in force rather
than a literal, so it survives that change.
