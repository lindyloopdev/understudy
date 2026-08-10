# Record a benched target the walk routed around

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Everything a request
moved on from goes on one record, `Excluded`, whatever kept it from serving", and the
health-transition rule above it, whose exception covers the transition itself and not
the per-request fact that a walk routed around a target.

`pickTarget` appends to its `skipped` list at one place: a target whose backend will
not resolve. A target skipped because it is *benched* — failing past the threshold,
or held to a re-admission moment — takes the `continue` and is never recorded, so it
is absent from the request's `Excluded` entirely.

The consumer's record for such a request shows a clean walk that served from a
sibling, as though the benched target were never a candidate. Reconciling that
against the `backend down` line means correlating a timestamped global record with
per-request ones, which is the work `Excluded` exists to save. The field's own doc
already promises otherwise: "A skipped backend appears on every request that routes
around it."

The behavior to pin: **should name a benched target the request routed around.**
