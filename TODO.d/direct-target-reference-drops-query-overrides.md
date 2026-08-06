# Cover what a request-named reference rejects

**Tag:** understudy / config

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the per-target
request-body overrides a target carries, the two ways a client can name what it
wants (a declared logical model, or a `backend/model` reference), and the domain
rules validated wherever a reference is accepted.

- **The rejection hands the caller strconv's words.** `openai/gpt-4?thinking=banana`
  answers `400` naming the reference — asserted at the request boundary, wording
  deliberately unpinned — but the message reads `invalid thinking value "banana":
  strconv.ParseBool: parsing "banana": invalid syntax`. `thinking` is understudy's
  own field with two legal values, so the message can say so without quoting the
  parser that happened to read it.

## Workaround

A logical model declared in `lindy.toml` whose single target carries the query can
be named instead of the reference. lindy's cost-reduction conformance sweep
(`probe-*` groups) uses this; it can be retired.
