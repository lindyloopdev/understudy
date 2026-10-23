# Record the model the upstream response names

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — `LogRecord` is
"where an operator reads what actually happened"; its `ModelUpstream` is the
name the serving attempt *requested*, which is not necessarily what served.

## Work

- Tests: a response with no `model` leaves `model_responded` empty.
- Consumer: lindy's `StepEnd.ServedModel` copies this record
  (lindy `TODO.d/served-model-differs-by-request-path.md`).
