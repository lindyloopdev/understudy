# Relay the Retry-After a 5xx advertised

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure table's
"advertised `Retry-After`" row, whose delay is "the upstream's own", and the
walk's verdict, which reads what each candidate advertised.

A `503` answering `Retry-After: 30` reaches the client as `502` with **no**
`Retry-After` at all. The provider already parses it — `withRetryAfter` wraps any
non-2xx carrying the header, so `classifyLimit` sets `hasRetryAfter` — and then
both forwarding branches in `errToResponse` gate on `sig.isRateLimit`, which is
`429` and nothing else. The upstream said when to come back, understudy captured
it, and dropped it on the floor.

- Forward an advertised delay whatever status carried it. The reject path already
  ignores `isRateLimit` and keys on the delay alone; the relay path should too.
- Do not confuse this with synthesizing one where none was sent — that is the
  separate gap in [[understudy-adaptive-coordinated-backoff]]. This is losing a
  value understudy already holds.

The verdict rule depends on it: a candidate's contribution is what it advertised,
so a delay dropped here is a delay the walk cannot offer the client either.
