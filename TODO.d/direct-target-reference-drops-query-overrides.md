# Cover what a request-named reference rejects

**Tag:** understudy / config

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the per-target
request-body overrides a target carries, the two ways a client can name what it
wants (a declared logical model, or a `backend/model` reference), and the domain
rules validated wherever a reference is accepted.

- **A non-boolean override answers 400.** `openai/gpt-4?thinking=banana` carries the
  invalid-value message. Only the reserved `thinking=true` case is asserted at the
  request boundary; the invalid-value rule is covered only through config load.

## Workaround

A logical model declared in `lindy.toml` whose single target carries the query can
be named instead of the reference. lindy's cost-reduction conformance sweep
(`probe-*` groups) uses this; it can be retired.
