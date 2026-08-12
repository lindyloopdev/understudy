# Say why a routed-around target is out

**Tag:** understudy / feature

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Everything a request
moved on from goes on one record, `Excluded`, whatever kept it from serving", and
`Called`, which carries whether understudy sent anything before excluding it.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the re-admission moment
such a record already reports.

A benched target on a request's `Excluded` says when it comes back and nothing of why
it went, so an operator reading one request still cannot tell a credential problem
from an outage without finding the request that demoted it.

The cause is on the entry: `targetHealth.lastError` is what the target answered when
it was last demoted, and `pickTarget` holds the entry when it decides the skip.

**Should say what a routed-around target last answered.**

Where it goes wants deciding. `Attempt` has an `UpstreamStatus`, but this attempt was
never made — `Called` is false — so a status there would describe a call this request
did not place. That argues for the message carrying both the bench and the cause, and
against reusing the numeric field for a previous request's answer.
