# Pin what a walk of nothing but stalls reports

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A list emptied by
misconfiguration answers 404, not 500" (which ending an exhausted walk gets) and
"The reason travels with the skip" (the record, not the client, is where a reason
lands).

- **Pin what a walk of nothing but stalls reports.** When every candidate stalls the
  walk exhausts, and the status it reports for the attempt it answers for lands on
  the record's own fields rather than on `Excluded`. Nothing asserts it, and a
  status-less error yields `500` from `yerrors` if anything derives one — which has
  happened once already, green.

## Out of scope

- **Failover for a target unusable at runtime.** Refused access or an
  unreachable host still demotes and is walked past; only *static* unusability is
  a skip. See [[fail-over-in-place-from-a-demoted-target]].
- **An unset credential under `auth = "auto"`.** The document is correct and the
  world is not supplying a key, which seeds health state rather than skipping —
  see [[auth-requirement-and-key-env-source]] and [[resolve-validate-split]].
- **Exported sentinels for the skip reasons.** No such backend, no registered
  handler, and no base URL all mean one thing to a consumer — this backend is
  unusable until an operator edits config — so none becomes public API.
- **Exposing the registered provider set** so a consumer can pre-check
  routability itself. Deliberately deferred until one needs it; a consumer can
  already enforce any rule it *can* see from its own `TokenValidator`.
