# Simplify the send loop

**Tag:** understudy / internals

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — displacement and the
first-content timeout, the two endings that re-send a request to the same
target.

`chatCompletions` runs to some 400 lines. Relaying a headered answer has its
own method (`relay`); the per-target send loop still sends one attempt and
waits to re-send after a displacement or a first-content timeout inline. Each
item below is a strict refactor: no behavior changes, the existing suite
stays green with no test diff.

## Work

- **Give sending and re-sending their own places.** Send once (acquire the
  slot, arm the first-content timeout, call `Chat`); wait to re-send (close
  the response, release the slot, await the queue, take a fresh context,
  re-read the body). The loop then reads as send, then relay, re-send, or
  fail.
- **One meaning per `attemptEnded` call.** It is called after a header
  arrives, after a failed send, and after the relay, with a different
  question each time; once the loop is split, each call has one place.
