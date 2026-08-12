# Say what a benched target is waiting for

**Tag:** understudy / feature

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Everything a request
moved on from goes on one record, `Excluded`, whatever kept it from serving", and
`Called`, which carries whether understudy sent anything before excluding it.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the re-admission moment
a benched target is held to, which is what such a record owes.

A benched target on a request's `Excluded` says only that it was not due. Two facts
the walk or the entry already holds would make the record worth reading, and neither
is recoverable from the other:

- **Should say when a routed-around target comes back.** `pickTarget` has the moment
  in hand — `nextReattempt` computes it to decide the skip — but `Attempt` has nowhere
  to put a time. Adding one is a change to a public type, so it wants deciding rather
  than assuming: a field, or the moment folded into the error's message.
- **Should say what the target last answered.** Needs the demoting error carried on
  `targetHealth`: [[say-why-a-backend-went-down]]. That half lands with it.

The walk must not name the *cause* from `readmitAt` alone: an advertised backoff and a
bench understudy synthesized both set it, and only the demotion knows which. That is
why the record says a target is not due rather than why it was benched.
