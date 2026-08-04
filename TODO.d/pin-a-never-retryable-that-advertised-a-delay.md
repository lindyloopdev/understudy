# Pin a never-retryable failure that advertised a delay

**Tag:** understudy / test

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure
table's "a `5xx` no retry can help" row, whose retry-delay column reads "none,
whatever it advertised", and the reject bands above it, which are what an
advertised delay would otherwise trigger.

The only never-retryable case answers a bare `501`, so the row's "whatever it
advertised" half is unpinned. Two cases prove it, both asserting a `502` with no
`Retry-After` header and no `retry_after_ms` in the body:

- `501` advertising a delay within the passthrough ceiling, which the widened
  relay would otherwise forward on the `502` — `isFatalUpstream` is true for it.
- `501` advertising a delay beyond the ceiling, which would otherwise answer
  `400 upstream_unavailable` with the delay in the body.

Both fail if `classifyLimit`'s never-retryable clear is removed, one per branch;
neither is covered by the existing terminal case, which reaches the clear through
`shouldReject` alone.
