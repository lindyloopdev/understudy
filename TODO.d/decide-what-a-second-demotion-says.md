# Decide what a second demotion in one streak says

**Tag:** understudy / design

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the health-transition
exemption, which grants understudy its own log line on the grounds that a transition
is an edge and the unit suppressing it is understudy's own streak.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion and half-open
re-admission, the states these records report.

A streak reports once — `demote` announces only when `downLogged` was not already set
— but a target can be demoted repeatedly within one: refused, then rate-limited, then
stalling, each a different cause with its own re-admission moment. So the first speaks
and the rest are silent, leaving four questions open and unasserted:

- **Should name the cause a target is out for now, not the one that started its
  streak.** A demotion announces for any entry whose streak was never reported, so a
  target refused and later stalling reads `no response header`. Which an operator
  should see is undecided — the same choice [[say-why-a-backend-went-down]] leaves
  open for the error, and both want one answer.
- **Should say when a benched target's return moves.** A re-bench overwrites
  `readmitAt` while `downLogged` stays set, so the last record's `readmit_at` outlives
  the moment it named. Answering the question above decides this one too: they are
  both about whether a second demotion speaks.
- **Should announce a demotion the streak never reported**, and **should stay silent
  when a target is demoted twice in one streak.** `demote`'s update path drives both
  outcomes and no test reaches it, for any of its three callers.

Whatever is decided, note what can guard it: these are per-request rules, so a test
can drive them today.

Which request wins the announcement when two demote the same target concurrently is a
separate question — [[decide-whether-transitions-are-ordered]].
