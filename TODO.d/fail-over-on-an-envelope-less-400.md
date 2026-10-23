# Fail over on an envelope-less 400

**Tag:** understudy / failover

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failure table
relays a `400` unchanged because "the *request* is at fault". That reading
assumes the body identifies the defect; a `400` that cannot say what was wrong
is not evidence about the request at all, and the walk already moves a target
that cannot serve what it is asked to serve ([[bind-a-conversation-to-its-target]],
the recovery-probing refusals) onto the next candidate.

## Work

- Observed 2026-10-21: zen go dropped `deepseek-v4-flash` from its catalog
  between one evening and the next morning. Its `400` for the removed model is
  a bare `{"model":"deepseek-v4-flash"}` — valid JSON, no `error` envelope, no
  message. `decodeErrorEnvelope` returns the zero value, the raw body becomes
  the message, and the walk answers the client a terminal
  `invalid_request_error`. `review-pair-thinking`'s `z-ai/glm-5.3-flash`
  fallback never fired; the beat died.
- Classify the condition precisely: an upstream `400` whose decoded envelope
  carries no message while the body is non-empty. That is a statement about the
  *target* (it cannot serve this model), not the request — the refusal family
  of the failure table, not the request-fault row.
- On that classification, exclude the target and continue the walk onto the
  next untried candidate, as a 401 is handled today. The shape match is the
  precedent of `isHistoryRejected`'s prose match, but keyed on absence (no
  envelope message) rather than wording, so a provider renaming its error text
  cannot outrun it.
- The failure mode in reverse is a provider that 400s genuinely malformed
  requests envelope-less: those get walked around too. The walk bounds the
  cost — each target is tried once, and the excluded attempts land on
  `LogRecord.Excluded` with their raw bodies, so the operator still reads what
  every candidate said.
- Tests: an upstream answering `400` with an envelope-less body moves the walk
  to the next candidate; the excluded attempt carries the raw body; with no
  candidate left, the client receives the `400` unchanged per the endings
  table; a `400` with a real envelope message still answers the client
  directly.
