# Let the walk carry the failure it answers with

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs out
of candidates answers for the request, not for its last target", and the failure
table's endings (a stall answers `504`, a refusal its terminal `400`, a model with
nothing usable the same words as one declaring no targets).

`canFailOver` is a lookahead that exists because the loop discards the failure that
caused the failover. Without it the walk would `continue`, `pickTarget` would report
nothing callable, and the request would answer `logical model %q has no targets` —
measured: `[a: 429/60s, b: unusable]` answered `404`, losing `a`'s real `429`.

Fail over until it cannot instead, and answer with the failure that got there. Only
a walk that never attempted anything — the first pick finding nothing callable — is
the model that has nothing to serve it.

- The walk has to carry its last failure across iterations, as it already carries
  `throttledErr` and `tried`. Today it cannot: the pick happens inside the
  `rewriteModel` closure at the top of each iteration, where the previous
  iteration's error is out of scope. That closure exists to rewrite a JSON body and
  has accumulated the walk's state; moving the pick out of it is the substance of
  this work.
- `canFailOver` and both its call sites then delete, and with them the recording
  they do: `pickTarget` logs each skip as it walks, so a walk that reaches the end
  records the remainder itself.
- Two cases are the guard, and neither should need to change: the sustained-`429`
  arm with an unusable candidate left untried, and a target failing past the
  terminal threshold with only an unusable one left. The stall and refusal arms of
  the same rule are not covered — [[degrade-past-a-misconfigured-backend]] — so this
  is a strict refactor only as far as those two reach.
