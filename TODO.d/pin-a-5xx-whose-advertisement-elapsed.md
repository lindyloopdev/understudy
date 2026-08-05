# Pin what a 5xx whose advertisement elapsed answers with

**Tag:** understudy / test

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure
table's "any other `5xx` advertising up to the passthrough ceiling" row, whose
delay column reads "the delay still outstanding", and the bare `5xx` row it falls
to when nothing is outstanding.

A `503` whose `Retry-After` names a moment already past answers `502` with no
`Retry-After` header at all — unlike the `429` leg, which synthesizes 60s. Only
the `429` leg is covered, so the relay's `isFatalUpstream` arm could start
forwarding a negative delta-seconds again without a test noticing.

One case in `TestChatCompletionsHandlesResponse`: a `503` carrying a past
HTTP-date, asserting `502` and response headers with no `Retry-After`.
