# Attribute each target demotion to the request that caused it

**Tag:** understudy / observability / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the demotion this
must be attributable to),
[DESIGN.md §Handler boundary](../DESIGN.md#handler-boundary) (`LogRecord` holds
only what understudy can supply, and the mount emits one entry per request).

A `backend down` transition should always have a corresponding logged request
that caused it. `FailedOver []Attempt` carries the targets a failover abandoned,
but an `Attempt` names only its backend and upstream model — not why it was
given up on.

- Carry the abandoned attempt's **upstream status** and **its own error** on
  `Attempt`, so a reader can tell a 429 apart from a 502 apart from a stall
  without correlating against another line. Rendering stays the mount's call.
- A demotion is attributable only once **every**
  `recordFailure`/`recordImmediateFailure`/`recordRateLimited` call site leaves
  its target somewhere in the record — in `FailedOver` if the request moved on,
  in the flat fields if there was nowhere to go. Audit the switch for a path
  that still demotes silently.

Keep `BackendName`/`ModelUpstream`/`UpstreamStatus` naming the attempt that
**determined the client's outcome** — the one whose response was relayed, or,
when every target failed, the one that produced `Err`. They stay flat and
greppable; searching "which model served this" must not become "read the tail of
an array".

**Out of scope: log ordering.** The mount emits at request end, so a concurrent
request's `pickTarget` can still log `backend down google` before the demoting
request's entry lands. This makes a demotion *attributable*, not necessarily
*preceded* by its cause in wall-clock order — do not write the acceptance
criterion as ordering.

Later, if it earns its place: per-attempt duration. It reads hardest on the
header-stall path, where an attempt abandoned after the full `headerStallGate`
of silence means something very different from one abandoned on an instant 429.

The `backend down` line naming an account sibling that never failed is a
separate defect in the health key's granularity, not the record's shape —
[[demotion-log-names-account-sibling]].
