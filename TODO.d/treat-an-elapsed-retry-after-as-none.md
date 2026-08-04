# Treat an elapsed Retry-After as no advertisement

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure table's
rows carrying "the delay still outstanding", and the walk's verdict, whose
contributions are compared to find the soonest.

`classifyLimit` sets `sig.retryAfter = time.Until(ra.RetryAfter())`, which is
negative whenever the advertised moment has already passed — an HTTP-date in the
past, `Retry-After: 0`, a sub-second prose delay. Nothing clamps it. Measured, an
upstream answering `Retry-After: Mon, 02 Jan 2006 15:04:05 GMT` reaches the client
as `Retry-After: -649568026`, on a `429` and on a `503` alike. The `503` half is new
exposure: until the relay widened past rate limits, a `5xx` carried no header at
all, so only the `429` path could emit one.

- Clear `hasRetryAfter` when the remaining delay is not positive, so the failure is
  treated as advertising nothing and falls to the synthesized path — which is what
  an elapsed deadline means.
- Fix it where the delay is computed, not where a caller renders it. Four readers
  take `sig.retryAfter` today: the two reject calls, the relayed header, and
  `recordRateLimited`, which benches a target until a moment already past. Guarding
  one leaves the rest — the reject body carries a negative `retry_after_ms` by the
  same route the header carries a negative delta-seconds, and a bench set behind
  now readmits immediately. The planned walk verdict will read it as well.

The verdict makes this worse than a malformed header: contributions are compared
to find the **soonest**, so a negative always wins. One stale advertisement from
any candidate would silently become the answer for the whole walk.
