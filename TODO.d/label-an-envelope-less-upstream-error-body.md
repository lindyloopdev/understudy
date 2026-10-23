# Label an envelope-less upstream error body

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the excluded
attempts on `LogRecord.Excluded` are "where an operator reads what actually
happened". A body relayed verbatim into an error message reads as the
upstream's own words when it is understudy's fallback doing the talking.

## Work

- When `errorFromResponse` (providers/openai) finds no `error.message` in the
  upstream body, today it substitutes the raw body verbatim. Observed
  2026-10-21, that surfaced as `upstream returned status 400:
  {"model":"deepseek-v4-flash"}` — indistinguishable from a provider that
  really writes `{"model": …}` as its error prose, and it sent the
  investigating operator (lindy's) down a false path before the bare echo was
  recognized as a body, not a message.
- Prefix the substituted body so the two cases read differently, e.g.
  `upstream returned status 400: no error envelope in body:
  {"model":"deepseek-v4-flash"}`. The body itself stays intact after the
  label; the `maxErrorBodyBytes` truncation still applies to it.
- A logged `upstream_error_message` attribute is already omitted when the
  envelope has none — this is only about the surfaced string.
- Tests: an envelope-less body renders with the label; an envelope with a
  message renders exactly as today.
- Related: [[fail-over-on-an-envelope-less-400]] decides what the walk *does*
  with such a body; this decides only what the operator is told.
