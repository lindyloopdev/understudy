# Honor an advertised backoff when nothing else is left

**Tag:** understudy / bug

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — an
advertised `readmitAt` supersedes the schedule, and a target carrying one is never
half-open-probed early. [DESIGN.md §Understudy](../DESIGN.md#understudy) — "emptied
by *health*, something is still worth attempting", which is what the fallback
exists to serve.

`pickTarget`'s exhausted-list fallback returns the last target its backend could
resolve, whatever its health — including one benched until an advertised moment that
has not arrived. Measured: a lone target demoted with `Retry-After: 60` is called
again one second later. That contradicts `pickTarget`'s own doc ("routed around
until that time … never half-open-probed early") and `recordRateLimited`'s promise
("not re-admitted before the backoff it advertised has elapsed").

The two rules disagree about what an exhausted list means. "Something is still worth
attempting" holds for a target the *schedule* is holding back — it may serve now.
It does not hold for one the upstream itself said would refuse until a stated time:
calling it early spends a request to be told what understudy already knows.

- Exclude a target under an unexpired `readmitAt` from the fallback, so the fallback
  offers only what health kept back on its own schedule.
- A list whose every candidate is benched until a known time then has nothing to
  attempt, and answers as one declaring no targets — the same ending unusability
  already gets.
- The case belongs beside "should serve from a benched candidate rather than answer
  for an unusable one that sorts after it", which today pins the permissive answer:
  it demotes with a `Retry-After`, so switching it to a bench with none keeps it
  driving the fallback it means to.
