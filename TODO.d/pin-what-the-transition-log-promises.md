# Pin what the transition log promises

**Tag:** understudy / test

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion
and re-admission, the pair the "backend down"/"backend up" records report.
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) —
the health map is shared by every in-flight request, so what is done while holding
it is every request's cost.

Two promises the transition log makes that no test would notice losing.

- **Should keep routing while the consumer's log sink blocks.** The emission was
  moved off the health lock precisely so a slow handler cannot hold other requests'
  walks behind it, yet every existing case would still pass if it moved back. Drive
  it with a handler that blocks on a channel for the first transition, issue a
  second request, and assert that request routes while the handler is still blocked.

- **Should log a backend down decided by a request that has already gone.**
  `pickTarget` emits through `context.WithoutCancel` so a committed demotion is not
  dropped with a departed client, but only the "backend up" half of that promise is
  driven: in "should log a transition the departed client discovered" the down record
  is written by an earlier request whose context is live. Measured: removing
  `WithoutCancel` from `pickTarget` breaks no test. Needs a walk that queues a down
  record after the client has left — a first target that cancels during its call,
  with a second already past its threshold and not yet due for a probe.

- **Should not log a backend up that was never down.** `dropHealth` reports false
  both for an entry whose down was never logged and for a target with no entry at
  all. Only the first is asserted, by "should not log a transition when a target
  recovers within the failover threshold". The second runs in every test — every
  healthy sibling serving a request — and nothing checks it.

Whether those records are ordered against each other is a separate open question:
[[decide-whether-transitions-are-ordered]].
