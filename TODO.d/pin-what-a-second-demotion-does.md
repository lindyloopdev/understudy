# Pin what a second demotion does

**Tag:** understudy / test

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the health-transition
exemption, which lets understudy log an edge on its own logger while per-request facts
go to the consumer's record.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion and half-open
re-admission, the states these records report.

A streak reports once: `demote` announces only when `downLogged` was not already set,
so a target refused, then rate-limited, then stalling produces one record. That stands
— the recurrence reaches an operator through the request records, which name the
target and its error on every probe that fails, at the rate understudy retries.

Both outcomes of `demote`'s update path are unasserted, for all three callers:

- **Should announce a demotion the streak never reported.** An entry can exist with
  `downLogged` false — a streak the walk accrued but no walk has yet routed around —
  and a demotion arriving then owes the record.
- **Should stay silent when a target is demoted twice in one streak.** The second
  demotion changes the cause and may move `readmitAt`, and says nothing.

The second leaves the edge record naming a return moment a later bench superseded.
[DESIGN.md §Understudy](../DESIGN.md#understudy) requires the record to say when the
target will be tried again, so the answer is not to drop it;
[[record-a-benched-target-the-walk-routed-around]] puts the *current* bench on every
request that routes around the target, which is where an operator reading a stale edge
would look next.
