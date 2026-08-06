# Retire lindy's workaround for request-named references

**Tag:** lindy / config

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the two ways a client
can name what it wants (a declared logical model, or a `backend/model` reference),
and the domain rules validated wherever a reference is accepted.

lindy's cost-reduction conformance sweep (`probe-*` groups) names a logical model
declared in `lindy.toml` whose single target carries the query, because a reference
carrying one could not be relied on. It can now name the reference directly.
