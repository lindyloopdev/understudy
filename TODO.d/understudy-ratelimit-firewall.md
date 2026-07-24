# Rate-limit reject: convert long 429s to a non-retryable typed error

**Tag:** understudy

**Design:** [UNDERSTUDY.md §understudy](../UNDERSTUDY.md#understudy).

A keyed band can be rate-limited. opencode's internal LLM retry honors
`Retry-After` essentially without bound, so a long 429 must be intercepted by
understudy *before opencode ever sees it*. This is the detection + fail-fast
mechanism under the escalate/defer path in [[understudy-model-groups]]
(§Priority/escalation).

Remaining work (all outside understudy now — the in-path reject is built): the
lindy-side consumer (§Required lindy-side change), the liveness watchdog's
refinements (§Liveness watchdog), and the spikes below.

## Why the reject must live in understudy

Verified against opencode source (`packages/opencode/src/session/retry.ts`,
`anomalyco/opencode@dev`):

- opencode honors a response's `Retry-After` up to
  `RETRY_MAX_DELAY = 2_147_483_647` ms (≈ **24.8 days**, the 32-bit setTimeout
  max). Without headers it self-caps at `RETRY_MAX_DELAY_NO_HEADERS = 30_000` ms.
- **No max-attempt ceiling** (retries via Effect `Schedule`). Corroborated by
  opencode issues #17648 (unbounded backoff, no circuit breaker) and #26350
  (silently stuck after retries).
- Hardcoded constants — **not configurable** via `opencode.json` (open feature
  request #3011). We cannot turn it down.

lindy **cannot** detect a long 429 from the event stream: opencode publishes
`session.error` only *after* the retry policy is exhausted (via `halt()`), which
for a long `Retry-After` + no ceiling effectively never fires. The SDK event union
has no retry/backoff/"waiting" variant; during the sleep the stream is simply
silent, indistinguishable from a long think/bash. So precise handling must be
proactive, in the one layer that sees the 429 + `Retry-After` synchronously:
understudy.

## The mechanism (verified feasible)

Two thresholds split the job:

- `Retry-After` **≤ threshold** → **pass the real 429/503 through.** opencode
  honors a reasonable backoff fine; the threshold sits above a sane provider
  backoff and only catches pathological waits.
- `Retry-After` **> threshold** → understudy returns **HTTP 400** with
  `retry_after_ms` and a status-derived type: a `429` → `upstream_rate_limited`,
  a 5xx (client-status `502`) → `upstream_unavailable`. Any other status carrying
  a `Retry-After` passes through (it is non-retryable, so opencode won't honor it).

Status and type do separate jobs:

- **Status (400) = retry-control aimed at opencode.** Verified: the Vercel AI SDK
  sets `isRetryable` from status (retryable only for 408/409/429/≥500), so
  400 → `isRetryable:false`. opencode's `retryable()` then hits
  `if (!isRetryable && !(status >= 500)) return undefined` → **non-retryable →
  opencode gives up immediately**, no sleep, no orphaned server-side loop. (A 5xx
  *would* be retried via the `status >= 500` clause — so it must be a non-retryable
  4xx; not 408/409/429; avoid 401, understudy's auth path.)
- **Envelope `type` = the reason aimed at lindy.** opencode's `MessageV2.APIError`
  carries `statusCode`, `responseBody`, `responseHeaders` into the published
  `session.error` event (matches the Go SDK `APIError.Data`). So type + reset time
  survive to lindy, which **dispatches on `type`, not status** —
  `upstream_rate_limited` is distinct from a garden-variety 400
  (`invalid_request_error`), so the status collision is a non-issue.

`upstream_rate_limited` is a *transport* cause (429), legitimately understudy's to
classify — consistent with the error-type contract in
[[understudy-error-envelope-type]]; not a Lindy domain concept. Reset time rides
in the envelope body (or a response header); either survives via
`ResponseBody`/`ResponseHeaders`.

Rejected alternative (out-of-band understudy→lindy abort signal): needs
understudy↔beat correlation, depends on `session.abort` interrupting opencode's
server-side sleep, and back-couples understudy into lindyd's beat lifecycle —
breaking its "library with no internal dependencies" property. Option 1 lets
opencode terminate *itself* and keeps understudy a pure proxy.

## Lindy-side dispatch (status path live-confirmed; type path deferred)

The status path works end to end and the pipeline guards it. `events.go` puts the
APIError `StatusCode` on the returned error (`yerrors`), `SDKRunner.Events`
forwards it (an earlier version silently dropped it on stream teardown), and `runBeat`
fails a terminal 4xx without retrying. `TestLiveReject` drives **real opencode**
against a mock reject and asserts the behavior: opencode delivers the 400 as a
`session.error` event (not a status-less `promptErr`), `HTTPStatus 400` survives
to the caller, and opencode gives up after the non-retryable 400 (no hang). It
runs wherever opencode is resolvable — locally on PATH, and in CI's `test` job,
which installs a pinned opencode so the live tests run rather than skip. The
`SDKRunner → runSeq → runBeat` glue is
covered by `lindyctx.Cause` (returns non-ctx errors verbatim) and `runSeq` (`%w`).

Still to do — **type-based** dispatch for smart remediation: carry the
`ResponseBody`/`ResponseHeaders`, then `runBeat` routes `upstream_rate_limited` →
escalate/defer (per [[understudy-model-groups]] §Priority). Deferred to that work.

Loose ends from routing the APIError to the error path:
- `lindy.AgentError` is now unproduced — its consumers (prompter runloop, director
  `apply`) and wire support (`ToPB`/`FromPB`) are dead; removing it is a gRPC
  event-vocabulary decision, not a silent delete.
- `MessageAbortedError` now surfaces as a beat error; may later want
  `ctx.Canceled`-style handling rather than a terminal error.

## Liveness watchdog

The watchdog core lives in `internal/opencode`: `idleGuard` wraps the beat event
stream in `SDKRunner.Events` and aborts with `errBeatIdle` after
`defaultBeatIdleTimeout` (10m). Still to do:

- Make the threshold operator-tunable (config plumbing into the `SDKRunner`
  field) and calibrate the 10m default from transcript inter-event gaps.
- Consider a longer budget between a `ToolCall` and its `ToolResult` — a silent
  long-running tool is the dominant legitimate gap, and the watchdog's blind spot
  if opencode does not stream part-updates during tool execution.

## Remaining spikes

- **Envelope survival — resolved (high confidence) via binary evidence; live
  confirmation outstanding.** The bundled opencode binary builds its published
  error by copying `message`/`statusCode`/`isRetryable`/`responseHeaders`/
  `responseBody` off the AI-SDK `APICallError` (whose `responseBody`/
  `responseHeaders` are populated for any non-2xx, incl. a 400) — exactly the Go
  SDK's `APIErrorData` shape. So a typed 400 + body/headers reaches lindy intact.
  Still want a live integration test asserting `APIError.Data.{StatusCode,
  ResponseBody, ResponseHeaders}` populated — cheap once the un-flatten carries
  the fields, so write it alongside that.
- **Body-substring caution.** `retryable()` greps `responseBody` for
  `FreeUsageLimitError`/`GoUsageLimitError` and *retries* those — but only *after*
  the non-retryable `return undefined`, so moot for a 400. Keep the reject's
  response body clear of those strings defensively.
- **Resume vs re-run after the 400** (shared with model-groups escalation): does
  opencode's `halt()` leave the session resumable (swap model on the live session,
  preserving partial work) or must lindy re-run the beat fresh? Can the SDK swap a
  live session's model? Mechanical, unresolved.
