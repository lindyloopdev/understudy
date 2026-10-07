# What a queue formation costs

**Tag:** understudy / admission / cost

**Design:** [DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— *What forms a queue* (canceling is free only before the header) and *The probe
discipline* (one unconfirmed request at the backend).

Forming a queue cancels outstanding requests and re-sends them when it admits
them, and the design rests that on a cost claim: "canceling it is free" for a
headerless request, "because a backend that admits before it answers has given
it no work".

## Can the same turn be cancelled twice?

Open question, mechanism first. `targetQueue.enter` arms one
`time.AfterFunc(admissionGracePeriod, …)` per admission, and each firing sweeps
everything outstanding. A re-sent attempt does not re-enter the queue, so it
arms no timer of its own — nor does a request re-queued after a bare 429
while another request at the target is producing content:
`requeueBehindContent` places it back in the waiting line without arming
one — but a later arrival's timer still sweeps it once
the queue has admitted it, and admission stamps a fresh `arrived`, so it is
exempt only until it ages past the grace period again.

Against that, the probe discipline looks protective: once a queue exists the
admission rule leaves one unconfirmed request outstanding, and a lone outstanding
request is the highest-ranked unconfirmed one, so the sweep spares it. The
reachable case would need two unconfirmed requests outstanding with the
re-sent one ranked below the other. Whether arrivals can produce that
arrangement is unestablished either way — and either answer is worth knowing,
because each repeat costs a whole generation on a contended backend.

## Work

- **Settle the repeat question.** Either demonstrate the arrangement that lets a
  turn be cancelled across two formations and bound it, or establish that the
  admission rule prevents it and record why, so the next reader does not have to
  re-derive it.
