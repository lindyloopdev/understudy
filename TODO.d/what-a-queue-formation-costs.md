# What a queue formation costs

**Tag:** understudy / admission / cost

**Design:** [DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— *What forms a queue* (canceling is free only before the header) and *The probe
discipline* (one unconfirmed request at the backend).

Forming a queue cancels outstanding requests and re-sends them when it releases
them, and the design rests that on a cost claim: "canceling it is free" for a
headerless request, "because a backend that admits before it answers has given
it no work". Nothing asserts the claim. The arrival trigger counts what the
target was sent
(`TestShouldAnswerWithTheResentAttemptWhenADisplacedTargetRespondsAnyway`); the
wait trigger has no test at all — it is currently unreachable
([[session-ordered-admission]], *Order turns already waiting at the target*).

Two arms of the sweep already carry inline markers in `understudy_test.go`,
below `TestShouldServeTheOlderSessionsTurnFirstWhenTheYoungerSessionsTurnArrivedEarlier`:
a headered request must not be taken, and one still inside its grace period
must not be either. Dropping either check leaves the whole suite green.

## Can the same turn be cancelled twice?

Open question, mechanism first. `targetQueue.enter` arms one
`time.AfterFunc(admissionGracePeriod, …)` per arrival that lands beside a
headerless request already past its grace period, and each firing sweeps
everything outstanding. A re-sent attempt does not re-enter the queue, so it
arms no timer of its own — but a later arrival's timer still sweeps it once
`check` has released it, and `check` stamps a fresh `arrived`, so it is exempt
only until it ages past the grace period again.

Against that, the probe discipline looks protective: once a queue exists the
release rule leaves one unconfirmed request outstanding, and a lone outstanding
request is the highest-ranked unconfirmed one, so the sweep spares it. The
reachable case would need two unconfirmed requests outstanding with the
re-sent one ranked below the other. Whether arrivals can produce that
arrangement is unestablished either way — and either answer is worth knowing,
because each repeat costs a whole generation on a contended backend.

## Work

- **Assert what a formation destroys**, not only the order it produces: the
  headered arm and the grace arm above, and the count of turns a single
  formation re-sends.
- **Settle the repeat question.** Either demonstrate the arrangement that lets a
  turn be cancelled across two formations and bound it, or establish that the
  release rule prevents it and record why, so the next reader does not have to
  re-derive it.
