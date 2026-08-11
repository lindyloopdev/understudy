# Pin what the transition log promises

**Tag:** understudy / test

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion
and re-admission, the pair the "backend down"/"backend up" records report.
[DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting) —
the health map is shared by every in-flight request, so what is done while holding
it is every request's cost.

One promise the transition log makes that no test would notice losing.

**Should keep routing while the consumer's log sink blocks.** The emission was moved
off the health lock precisely so a slow handler cannot hold other requests' walks
behind it, yet every existing case would still pass if it moved back. Drive it with a
handler that blocks on a channel for the first transition, issue a second request, and
assert that request routes while the handler is still blocked.

Whether the records are ordered against each other is a separate open question:
[[decide-whether-transitions-are-ordered]].
