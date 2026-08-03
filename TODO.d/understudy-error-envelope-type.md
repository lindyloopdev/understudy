# Move the failure-translation table to the handler boundary

**Tag:** understudy / api

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "One table maps an
upstream failure to what the client is told", the status/`type` split the
rate-limit reject rests on, and the `5xx/401/403` body obfuscation the table has
to keep.

The mapping lives in the provider today (`typeForStatus` in `providers/openai`),
and whatever `type` the upstream named is relayed ahead of it. Both contradict
[`providers/providers.go`](../providers/providers.go) — "what the client is shown
is derived at the handler boundary, never set by a provider" — so extending the
provider's table would deepen the split rather than close it.

- Give the provider an interface that reports the **condition** it observed
  rather than an envelope type. `ErrorTyper` asks for the answer; a provider can
  only honestly supply the observation.
- Move the mapping to the handler boundary as one table over condition and health
  state, deriving status, `type`, and `Retry-After` together — they are decided by
  the same facts and currently diverge across three sites.
- Answer for the request, not for its last candidate. The walk still renders
  whichever error came last, so `[a: 429 for 60s, b: 401]` answers
  `upstream_refused` on `b` while `a` is merely throttled and will serve once its
  delay elapses. §Understudy requires the most-optimistic verdict over the
  attempts already on `Excluded`.
- Settle the row still open: what an overloaded upstream carries once
  `overloaded_error` stops arriving by passthrough, and whether it stays
  distinguishable from a faulted one.
- Publish it. There is no user-facing doc yet — see [[documentation]], whose
  README and library-doc restructure are where a consumer reads this.

Nothing depends on passthrough today: live lindy dispatches on envelope `type`
nowhere, and [[understudy-ratelimit-firewall]]'s planned dispatch already expects
understudy-derived types. That window closes when the firewall lands.

## Also here

- **De-duplicate the type wrapper.** `typedError` (understudy) and `errorTypeError`
  (openai) are the same shape in two packages; extract to a shared spot once a
  third user appears (neither package should import the other's).

- **(open) Fuller upstream-error fidelity.** `errorFromResponse` flattens the
  upstream `message` into `"upstream returned status N: <message>"` and drops the
  upstream `code`/`param`. Carry `code`/`param`, or pass the upstream error body
  through verbatim, if a behavior calls for it.
