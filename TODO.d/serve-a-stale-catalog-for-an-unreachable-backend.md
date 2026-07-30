# Serve a stale catalog for a backend that cannot answer now

**Tag:** understudy / models / availability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Two endpoints, two
answers" (the listing does not fail because a backend could not be reached, and
emptiness is a valid answer), "Least degradation: a backend understudy cannot use
costs that backend, not the request", and "Operator and caller learn different
things" (the reason is the operator's fact).

A backend skipped from `/v1/models` drops out of the listing entirely, so one 429
or a brief outage silently shrinks what understudy advertises it can serve. The
caller cannot tell a model that no longer exists from one whose backend is having
a bad minute. Caching each backend's last successful catalog would let a
transiently unreachable backend keep contributing its known models instead.

Least degradation is what makes this attractive and also what bounds it: a stale
entry must never make understudy advertise a model it cannot route to, so decide
what the cache is allowed to outlive.

- **What invalidates an entry.** A refused credential and a removed backend are
  facts a stale catalog would lie about, unlike a 429 or a 5xx. Distinguishing
  them means the listing reads the failure's kind, which the endpoint deliberately
  does not do today ("whatever its cause").
- **How long an entry survives** an upstream that never comes back, and whether
  age is exposed to the caller or only to the operator's log.
- **Where the cache lives.** Per-process today; a shared understudy daemon would
  hold one catalog per account rather than one per process —
  see [[understudy-limiter-ceiling-ratchet]] for the same process-local-versus-shared
  question on the limiters.
- **Whether the chat path may consult it.** Chat asks understudy to *serve this*,
  where failure is failure — a cached catalog must not become a routing decision.
