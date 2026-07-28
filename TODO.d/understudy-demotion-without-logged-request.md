# Attribute each target demotion to the request that caused it

**Tag:** understudy / observability / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the demotion this
must be attributable to),
[DESIGN.md §Handler boundary](../DESIGN.md#handler-boundary) (`LogRecord` holds
only what understudy can supply, and the mount emits one entry per request).

Set the flat `UpstreamStatus` for a **failed** final attempt.
`setLogUpstreamStatus` sits past the error check, so a request whose last target
failed records the status it failed with as `0`. An abandoned attempt says why it
was given up on; the attempt that ended the request reports only *that* it
failed, not what happened.

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
