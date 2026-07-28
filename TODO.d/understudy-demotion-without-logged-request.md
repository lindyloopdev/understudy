# Attribute each target demotion to the request that caused it

**Tag:** understudy / observability / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the demotion this
must be attributable to),
[DESIGN.md §Handler boundary](../DESIGN.md#handler-boundary) (`LogRecord` holds
only what understudy can supply, and the mount emits one entry per request).

A `backend down` transition should always have a corresponding logged request
that caused it. Observed anomaly at the start of a review run: google was
demoted (`backend down google`) with **no logged google request** preceding it
— the only earlier entry was `POST /session`.

The cause is that `LogRecord` holds one attempt per request while a failover
makes several. `chatCompletions` resets `parsedBackendName` at the top of each
pass and `setLogBackendName` overwrites the record, so a request that fails on
google and replays onto a sibling logs only the sibling. The google attempt that
called `recordFailure`/`recordRateLimited` never appears, but the demotion it
caused does.

Give the record room for the attempts a failover abandoned, without displacing
the fact operators actually search on:

- `BackendName`/`ModelUpstream`/`UpstreamStatus` keep naming the attempt that
  **determined the client's outcome** — the one whose response was relayed, or,
  when every target failed, the one that produced `Err`. These stay flat and
  greppable; searching "which model served this" must not become "read the tail
  of an array".
- Add `FailedOver []Attempt` alongside them for the attempts abandoned before
  that one, each carrying its backend, upstream model, upstream status and its
  own error. Empty for the overwhelming majority of requests.
- Every `recordFailure`/`recordImmediateFailure`/`recordRateLimited` call site
  must then leave its target somewhere in the record — in `FailedOver` if the
  request moved on, in the flat fields if there was nowhere to go. Cover the
  header-stall path (`errHeaderStall` demotes and replays) as well as the
  `sustainedRate` replay.

Additive, so nothing an embedder reads today breaks. Rendering is the mount's
call — a single `failed_over=…` attribute keeps it one line.

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
