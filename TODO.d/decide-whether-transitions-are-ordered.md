# Decide whether a target's transitions are ordered

**Tag:** understudy / design

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion
and half-open re-admission, the pair the "backend down"/"backend up" records report.
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) —
the health map is shared by every in-flight request, which is why the emission was
moved off the lock in the first place.

Which transition a target went through is decided under `s.mu`; when its record
reaches the consumer's sink is not. A walk marks `downLogged` under the lock and
emits "backend down" after releasing it, so a request that succeeds on that target
in the window between can run `clearFailure`, observe `downLogged`, and emit
"backend up" first. An operator tailing the log sees a target come up before it
went down.

Nothing promises otherwise: `clearFailure`'s doc claims the pair is symmetric — an
up only where a down was logged — which stays true, because that is decided under
the lock. So this is a question to settle, not a bug to fix:

- **If order is part of what the log means**, both emissions need one serialization
  point outside `s.mu` — a per-server queue drained in decision order. That is a new
  synchronization primitive on a path that currently has none, and it must not
  reintroduce the blocking-sink problem that moved emission off the lock.
- **If order is best-effort**, say so where an operator will read it, and keep the
  cheap deferred emission.

Settle it before adding a third transition record; a rule inferred from two is already
ambiguous. Whatever serializes them must stay off the health lock — a case pinning that
a request still routes while the sink holds a record will fail if it does not.
