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

- **Move the pick out of the `rewriteModel` closure first.** The walk has to carry
  its last failure across iterations, as it already carries `throttledErr` and
  `tried`, and today it cannot: the pick happens inside a closure whose job is to
  rewrite a JSON body, where the previous iteration's error is out of scope.

  Two attempts at inverting the control flow in place have failed, each ruling out a
  shape. Both are worth knowing before a third.

  **Carrying a bare error does not work.** The throttle swap decides the *final*
  answer, so it belongs at the stop; left per-hop it fires on every failing
  candidate. It cannot simply move to the stop either: it tests the **raw** error
  (`isAccessRefused`, `HTTPStatus == 501`) while what must be carried is the
  **classified** one, and `clientFacing` rewrites a `501` into a `502` wrapped in
  `neverRetryableError`, so the test cannot be made on the carried value.

  **Carrying both, bundled, does not work either — and the reason is `Excluded`.**
  A bundle (raw, classified, target, log fields) fixes the swap, but moves the
  question to *when* a candidate is recorded as one the request did not serve from.
  Record on **departure**, as today, and a walk that turns out to have nowhere left
  to go has already recorded the candidate whose failure then answers — the request
  is logged as not having served from the thing it answered with. Record on
  **arrival**, once the next pick succeeds, and the ordering breaks: `pickTarget`
  logs the skips it walks past *during* that pick, so the abandoned candidate lands
  after them, against §Understudy's "recorded in the order they were walked".

  Departure-ordering and arrival-correctness conflict while the pick is inside the
  closure, because the walk cannot see its own sequence from there. That is what
  makes moving the pick out the prerequisite rather than a tidy-up.

  Guards that caught these: "should name the candidate a refused request answered
  from", "should serve from a benched candidate rather than answer for an unusable
  one that sorts after it", "should record the refused target a request did not serve
  from when an earlier throttle answers for it", and "should record an abandoned
  target and an excluded one in the order it walked them".
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

  A client disconnect is not the case to watch: `responseStatus` matches
  `context.Canceled` before it reads any status, so that request answers `499`
  whether or not the reading fires.
- With the error pre-classified, only `terminalFailure` is left to run at the stop,
  needing `answering` and `remaining`. That is much smaller than recomputing the
  terminal block, which is what "answer with the failure that got there" sounds like.
- **Verify, do not assume:** `setLogUpstreamStatus` fires only on the stopping path
  today. Firing it per attempt looks equivalent — later attempts overwrite earlier
  ones, and the success path overwrites with the response's status — but that was
  reasoned, not measured. Measure it before relying on it.
- Seven cases are the guard, and none should need to change: each replay arm with an
  unusable candidate left untried — a sustained `429`, a refusal, a pre-header stall,
  a fatal `5xx` on a demoted pick's half-open probe — a target failing past the
  terminal threshold with only an unusable one left, a client that goes away
  mid-walk, and a host's own cancellation cause raised mid-walk.
  The last is the one that sees where classification happens: substituting a dead
  context at the classification point turns its `503` into a `502`. A case that needs
  editing is the signal this changed behavior rather than shape.
