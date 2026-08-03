# Record a client that left before the answer reached it

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — error rendering is
understudy's, and the failure table names what each answer is; the telemetry
paragraph ("understudy owns its telemetry record") bounds what may go on
`LogRecord`, which is where this fact would land.

`writeErrorEnvelope` discards the encoder's error, so a client that disconnects
between `WriteHeader` and the body leaves no trace: the log says understudy
answered, and nothing says the answer went nowhere. The streaming path already
does better — `io.Copy`'s error returns to `errToResponse`, whose already-wrote-
header guard logs it — so the two paths disagree about the same event. 499 is the
status understudy already uses for a client that closed
(`statusClientClosedRequest`, and `responseStatus` maps `context.Canceled` to it).

Two things have to be settled before this can be written, and neither is obvious:

- **Where it goes.** `setLogError` assigns `Err` outright, so calling it here
  would erase the cause the request had already recorded — for a refusal that is
  the operator's only copy, which the disclosure rule promises they keep. Wrapping
  preserves both; a new field would have to answer why response status is on
  `LogRecord` when §Understudy excludes generic HTTP facts from it by name.
- **Whether it is worth a log line at all.** What an operator does with "the
  client hung up before reading its 400" is unclear; it explains a missing
  client-side error and little else. If the answer is nothing, the fix is a
  comment saying the discard is deliberate, not a new field.

Both writers that predate the refusal work discard it the same way, so this is
not new — it is only newly visible now that one place owns the envelope.
