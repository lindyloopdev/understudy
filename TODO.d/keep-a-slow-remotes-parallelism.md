# Keep a slow remote's parallelism under the admission queue

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — a queue forms when a request
waits past the grace period with no header, and releases one unconfirmed request
at a time.

A remote that is slow to send headers under load but can serve many requests at
once forms a queue like a one-slot local server, and is then sent one new
request at a time. DeepSeek took 12.5s to its header under load: 20 new sessions
would start one per first-content latency, several minutes for the last, though
it may have served all 20 together. Accepted for now: the only slow-header
evidence (the same DeepSeek load) coincided with hung requests, where sending one
at a time lost nothing.

## Work

- Revisit when a remote is observed with slow headers *and* spare capacity.
- Candidates, each without a per-backend flag: learn the grace period per key
  from observed header latency; or let the count of unconfirmed requests grow
  while confirmations keep arriving (a learned window per key, like the
  concurrency cap's growth).
- Test: a backend that confirms concurrent requests promptly despite slow
  headers is not held to one unconfirmed request.
