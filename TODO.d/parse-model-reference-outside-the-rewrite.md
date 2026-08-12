# Parse the model reference outside the rewrite callback

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the failover walk and
the per-attempt target selection the callback drives), [DESIGN.md §Handler
boundary](../DESIGN.md#handler-boundary) (error rendering is understudy's, read from
the error itself — so resolution failures keep carrying their own status rather than
becoming a domain-error enum mapped at the handler).

`chatCompletions` resolves a model reference inside the closure it hands
`rewriteModel`. The closure returns one string but its real output is five
assignments to enclosing variables — `requestedModel`, `logicalTargets`, `chosen`,
`parsedBackendName`, `upstreamModel` — reset at the top of every failover
iteration. Nothing about interpreting a model name needs a JSON body, yet none of
it can be exercised without one.

Separate what the name *means* from what serves it:

- **Interpreting the name is pure.** Which of four things did the client name —
  nothing, a declared logical model, a `backend/model` reference, or a bare name
  matching neither? No health, no clock, no I/O. It should return a value a table of
  strings can test.
- **Selecting a target is stateful** and already extracted: `pickTarget` consults
  health, records skips, stamps `lastProbe`.
- **Rewriting the wire field is transport**, and is all the callback should do.

The replacement string is not known until a target is selected, and selection is
stateful, so selection stays *called from* the callback — it just stops being
*written in* it.

`resolveError` should disappear with this. It exists only to mark an error as
having come from inside the callback so `chatCompletions` returns it verbatim
instead of wrapping it as a malformed body; with parsing outside, there is nothing
to distinguish and the `errors.AsType` unwrap goes too.
