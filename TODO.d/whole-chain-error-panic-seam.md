# Extend the error-render seam to wrap the whole handler chain

**Tag:** understudy / refactor

**Design:** [DESIGN.md §Handler boundary](../DESIGN.md#handler-boundary)
(error rendering is understudy's, read from the error itself).

Today error rendering and panic recovery are bolted onto each route leaf, so
parts of the chain above the leaves bypass understudy's error-value seam.
`New` wires `withStatusRecorder(authMiddleware(v, mux))`; `recoverPanic` and
`errToResponse` wrap only the leaf `apiHandler`s. Consequences:

- A panic in `authMiddleware` or in the `ServeMux`'s own routing sits inside no
  `apiHandler`, so it escapes `recoverPanic` and unwinds into net/http's
  default — a bare 500 with **no `yerrors` stack in the request log** and no
  envelope. It never reaches the render seam.
- Auth-path errors are rendered by a separate `writeJSONError` call in
  `authMiddleware`, not by the same `errToResponse` path route errors take.

The right model (already how `recoverPanic` works per-route): a panic is
converted to a `yerrors.WithHTTPStatus(500, …)` **value** and returned through
the `apiHandler` error channel, then rendered by `errToResponse` like any other
error — the same single seam that carries upstream-backend errors (429/
`upstream_rate_limited`, `upstream_unavailable`, the `typedError`/`resolveError`
envelopes). No parallel panic path that writes its own response. This item makes
that seam **total** — reaching auth and routing — rather than leaf-only.

Proposed mechanism: adopt `gitlab.com/flimzy/httpe`. `HandlerWithError` +
`ToHandler`/`ToHandlerWithError` ferry the `func(w,r) error` shape across the
`ServeMux` boundary (returned error → sentinel panic → recovered back to a
value; a real panic is not the sentinel, so it re-panics past
`ToHandlerWithError` to an outer recover). Target shape:

- routes register as `httpe.ToHandler(httpe.HandlerWithErrorFunc(handler))`
- `errToResponse`'s body (`render`) and `recoverPanic` become
  `HandlerWithError → HandlerWithError` middlewares wrapping the whole chain
- `authMiddleware` returns errors instead of calling `writeJSONError` directly
- `withStatusRecorder` stays outermost so `render`'s committed-response check
  (the mid-stream case) still sees the recorder

## Remaining work

- Strict refactor first: hoist error rendering + panic recovery onto the
  whole chain with **zero behavior change**, guarded by the existing
  `TestChatCompletions*` envelope/status tables.
- Then the additive behavior: an outer `recoverPanic` covers auth/routing
  panics (converted to a 500-carrying error, logged with stack, rendered as a
  `server_error` envelope). Supersedes the per-route `recoverPanic`.

Out of scope (discuss separately on the lindy side): whether lindy's own
`httpd` chain adopts httpe, and whether httpe becomes the standard HTTP
error/panic idiom across both flimzy-authored HTTP surfaces.
