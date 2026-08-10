# Give the schedule kind one home

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the two
schedules a failing target can be held to, which is what this classification names.

`h.readmitAt.IsZero()` is read at two sites for the same question. `nextReattempt`
uses it to decide *when* a target is due; `noteBackendDown` re-tests it to decide
*which* schedule to name in the record — `advertised backoff` with `readmit_at`, or
`awaiting recovery probe` with `next_probe`. A change to how the kind is decided has
to land at both, and the walk site alone would leave the record labeling correctly
timed benches with the wrong cause.

Return the kind with the moment, so the record reads the classification rather than
re-deriving it.

**Do this after [[name-a-synthesized-bench-as-understudys-own]], not before.** That
entry replaces the `readmitAt` inference with a cause recorded at demotion, which is
the same discriminator seen from the other end — building a kind on top of `readmitAt`
first means designing it twice, and the second design throws the first away.
