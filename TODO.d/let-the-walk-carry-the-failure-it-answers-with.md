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
- **Do not classify a carried error against a dead context.** `clientFacing(ctx, err)`
  opens with `errors.Is(err, context.Cause(ctx))`, which passes an error through
  untouched when it *is* the cancellation, instead of flattening it to `502`. The
  cause that matters is not always `context.Canceled`: a host cancels the *request*
  context with its own — `WithHTTPStatus(503, "lindyd: shutting down")` is how a
  consumer renders shutdown without understudy knowing what shutdown is, and a
  deadline gives `DeadlineExceeded`. Measured at the single call site: two cases
  reach it with a cause that is neither.

  Those causes live on the **request** context, which outlives every hop, and each
  hop's child inherits them. `cancel(nil)` overwrites a child's cause with
  `context.Canceled` only when the parent has none. So classifying in the iteration
  that failed is correct, and so is classifying later against a *fresh* child — but
  classifying against the **dead** child of an earlier hop is not: its cause has been
  overwritten, the reading stops matching, and a host's `503` becomes a `502`. Stash
  `err` and answer with it after the loop while still holding the old `ctx` and that
  is exactly what happens.

  The reading is guarded — "should surface a bare cancellation cause as 5xx" and
  "should surface a cancellation cause's own HTTP status" both fail if it is removed
  — but only for a single hop, where in-iteration and after-the-loop are the same
  moment. The missing guard is a shutdown-style cause during a multi-target walk.

  A client disconnect needs no guard: `responseStatus` matches `context.Canceled`
  before it reads any status, so that request answers `499` either way — verified by
  disabling the reading, which leaves the mid-walk disconnect case byte-identical.
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
  The guard the reading needs is a host-supplied cause — a `503` shutdown, say —
  arriving during a walk that has more than one candidate, so that in-iteration and
  after-the-loop classification give different answers. The mid-walk disconnect case
  cannot see that difference.
