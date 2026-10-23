# Record the model the upstream response names

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — `LogRecord` is
"where an operator reads what actually happened"; its `ModelUpstream` is the
name the serving attempt *requested*, which is not necessarily what served.

## Work

- `LogRecord.ModelUpstream` is set from the resolved target (`setLogModels`)
  before the request is sent, and no code reads the response's `model` field. A
  request that names a model the upstream has since aliased or replaced is
  recorded under the name it sent. Observed 2026-10-21 (lindy's
  `~/.lindy/logs/proxy.log`): `opencode-go/deepseek-v4-flash` answered 400 at
  08:40 and 08:49 once opencode-go dropped the model, then 267 requests for the
  same name returned 200 through 11:32, all logged `model_upstream` =
  `deepseek-v4-flash`. Whether it was served as itself or as another model is
  not recoverable.
- Capture the `model` the upstream's response reports (non-streaming body and
  streaming chunks; confirm both carry it for each provider) and add it to
  `LogRecord` as its own field, leaving `ModelUpstream` as the requested name.
  Absent when the response names none.
- Log it on the request line next to `model_upstream`.
- Tests: a response naming a different model than was requested records both;
  a response with no `model` leaves the field empty.
- Consumer: lindy's `StepEnd.ServedModel` copies this record
  (lindy `TODO.d/served-model-differs-by-request-path.md`).
