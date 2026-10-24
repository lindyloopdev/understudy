# Record the model the upstream response names

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — `LogRecord` is
"where an operator reads what actually happened"; its `ModelUpstream` is the
name the serving attempt *requested*, which is not necessarily what served.

## Work

- Tests: a response that names no `model` in model-inspecting cases leaves
  `model_responded` empty (e.g. a non-streaming body without the field, or one
  whose field appears after `maxBodyPrefixBytes`).
- Consumer: lindy's `StepEnd.ServedModel` copies this record
  (lindy `TODO.d/served-model-differs-by-request-path.md`).
