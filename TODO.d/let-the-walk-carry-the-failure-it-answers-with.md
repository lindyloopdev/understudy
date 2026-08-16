# Let the walk carry the failure it answers with

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs out
of candidates answers for the request, not for its last target", and the failure
table's endings (a stall answers `504`, a refusal its terminal `400`, a model with
nothing usable the same words as one declaring no targets).

**Verify, do not assume:** `setLogUpstreamStatus` fires only on the stopping path
today. Firing it per attempt looks equivalent — later attempts overwrite earlier
ones, and the success path overwrites with the response's status — but that was
reasoned, not measured. Measure it before relying on it.
