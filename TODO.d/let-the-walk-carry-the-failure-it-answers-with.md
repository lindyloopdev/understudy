# Let the walk carry the failure it answers with

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A walk that runs out
of candidates answers for the request, not for its last target", and the failure
table's endings (a stall answers `504`, a refusal its terminal `400`, a model with
nothing usable the same words as one declaring no targets). Also the refusals table's
`client disconnected | 499` and the rule above it: the status is read from the error
itself, a status-less `context.Canceled` normalized to `499`.

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
- **Carry the client-facing error, not the raw one.** `clientFacing(ctx, err)` opens
  with `errors.Is(err, context.Cause(ctx))`, which asks whether this attempt failed
  because *this attempt's* context was cancelled — the client hanging up, or
  understudy's own stall gate. Each iteration builds its own `ctx` and cancels it
  before continuing, so asking later is not merely riskier, it is unanswerable: the
  cause that identified it is gone, and a dead context reports `context.Canceled`
  for every carried error. §Understudy already requires the reading, since the status
  is a property of the error. Classify in the iteration that failed; carry the result.
- With the error pre-classified, only `terminalFailure` is left to run at the stop,
  needing `answering` and `remaining`. That is much smaller than recomputing the
  terminal block, which is what "answer with the failure that got there" sounds like.
- **Verify, do not assume:** `setLogUpstreamStatus` fires only on the stopping path
  today. Firing it per attempt looks equivalent — later attempts overwrite earlier
  ones, and the success path overwrites with the response's status — but that was
  reasoned, not measured. Measure it before relying on it.
- Four cases are the guard, and none should need to change: each replay arm with an
  unusable candidate left untried — a sustained `429`, a refusal, a pre-header stall
  — and a target failing past the terminal threshold with only an unusable one left.
  A case that needs editing is the signal this changed behavior rather than shape.
  None of them involves a client that disconnects mid-walk, so the reading above is
  unguarded — a case for it belongs with the work.
