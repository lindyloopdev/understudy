# Report every omission the same way

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Operator and caller
learn different things" (the skip reason reaches the operator through the request's
`LogRecord`), "The reason travels with the skip" (each reason reaches the
operator through `Excluded`, never the client), "A list emptied by misconfiguration answers
404, not 500" (which ending each exhaustion gets, and that the rule is about usable
targets however the list emptied), and "Two endpoints, two answers" (emptiness is a
valid answer for the listing whatever its cause).

- **Pin what a walk of nothing but stalls reports.** When every candidate stalls the
  walk exhausts, and the status it reports for the attempt it answers for lands on
  the record's own fields rather than on `Excluded`. Nothing asserts it, and a
  status-less error yields `500` from `yerrors` if anything derives one — which has
  happened once already, green.

- **Pin that a re-walked target is recorded once per pass.** `pickTarget` re-walks
  the candidate list from the start on every failover, so targets `[broken, limited,
  good]` where `limited` fails over yield `broken, limited, broken`. No case
  exercises the repeat. Say in the `Excluded` doc that a target walked past twice is
  recorded twice — the field reads as a set, and a set does not repeat.

- **A listing's failed catalog fetch reaches only the log.** The listing's two
  omission paths answer a consumer differently: a backend understudy cannot use is
  recorded on `Excluded`, while one whose catalog fetch fails is written to
  `s.logger` at ERROR and left off the record. Both are the same fact from the
  consumer's side — this backend is missing from your listing, and here is why.

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
