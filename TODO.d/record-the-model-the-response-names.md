# Record the model the upstream response names

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — `LogRecord` is
"where an operator reads what actually happened"; its `ModelUpstream` is the
name the serving attempt *requested*, which is not necessarily what served.

## Work

- Streamed responses are not scanned: `responseModelReader` settles a body
  that is not a JSON object by its first non-whitespace byte, so an SSE
  response records no `model_responded`. Capture the `model` the streaming
  chunks carry, and confirm streaming chunks carry it for each provider.
- Tests: a response with no `model` leaves `model_responded` empty.
- Consumer: lindy's `StepEnd.ServedModel` copies this record
  (lindy `TODO.d/served-model-differs-by-request-path.md`).
