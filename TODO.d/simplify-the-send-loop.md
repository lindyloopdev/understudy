# Simplify the send loop

**Tag:** understudy / internals

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — displacement and the
first-content timeout, the two endings that re-send a request to the same
target.

`chatCompletions` runs to some 390 lines. Each target's sends go through an
`attempt`, whose `send`, `relay`, and `waitToResend` each do one job; the
item below is a strict refactor: no behavior changes, the existing suite
stays green with no test diff.

## Work

- **One meaning per `attemptEnded` call.** It is called after a header
  arrives, after a failed send, and after the relay, with a different
  question each time; with the loop split, each call can have one place.
