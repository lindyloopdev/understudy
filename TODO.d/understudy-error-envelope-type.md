# Error envelope `type` — finish the carried-type contract

**Tag:** understudy / quick

**Design:** [UNDERSTUDY.md §understudy](../UNDERSTUDY.md#understudy).

Errors carry their OpenAI envelope `type` via an `ErrorType() string` method;
the response seam reads it (`errorType(err)` via `errors.AsType`) and falls back
to `server_error` only when no error in the chain carries one. The type is set
by whoever has the most context: the openai provider classifies upstream errors
(`errorFromResponse`: the upstream's `type`, else `typeForStatus(status)`), and
understudy tags its own errors at construction (`typedError`). 401 is
`authentication_error` via `authMiddleware`.

## Remaining work

- **Extend the provider's status→type table.** `typeForStatus` maps
  `429 -> rate_limit_error` and `400`/`404 -> invalid_request_error` so far;
  other statuses default to `server_error`. Add remaining OpenAI conventions
  (403 -> permission_error, …) as behaviors drive them. `401` is deliberately
  deferred: for a proxy an upstream 401 means
  understudy's *configured* backend key is bad, not the client's token, so
  `authentication_error` would mislead the client — it stays `server_error`
  until that semantics is decided.

- **De-duplicate the type wrapper.** `typedError` (understudy) and `errorTypeError`
  (openai) are the same shape in two packages; extract to a shared spot once a
  third user appears (neither package should import the other's).

- **(open) Fuller upstream-error fidelity.** `errorFromResponse` flattens the
  upstream `message` into `"upstream returned status N: <message>"` and drops the
  upstream `code`/`param`. Carry `code`/`param`, or pass the upstream error body
  through verbatim, if a behavior calls for it.
