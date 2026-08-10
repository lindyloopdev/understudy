# Name a synthesized bench as understudy's own

**Tag:** understudy / bug

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — a demoted
target is routed around until its re-admission moment, which is what the "backend
down" record reports. [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure
table's pre-header stall, which understudy answers for rather than relaying.

`noteBackendDown` infers its reason from `readmitAt` alone: non-zero reads as
`advertised backoff`. But a pre-header stall is demoted through
`recordRateLimited(chosen, synthesizedStallBackoff, …)`, so a target that answered
*nothing* is logged as though the upstream asked to be left alone. The two are
opposite operator situations — one upstream is talking and setting terms, the other
has gone silent — and the record cannot tell them apart.

Carry the demotion's cause on `targetHealth` where the demotion is written, rather
than re-deriving it from a field that two paths set, and give the stall its own
reason.

The behavior to pin: **should say understudy synthesized the bench when the upstream
answered nothing.** A stalled attempt is already driven by "should record a stalled
attempt as having answered nothing", so the setup exists.

Note [[say-why-a-backend-went-down]] wants the demoting error on the same record. If
that lands first, the cause may fall out of it rather than needing its own field.
