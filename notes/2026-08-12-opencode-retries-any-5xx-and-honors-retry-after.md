# opencode's retry loop honors Retry-After on any status — but the agent turn never runs it

**Date:** 2026-08-12
**Subject:** whether understudy must convert a retryable upstream failure to
`429` for opencode to back off, or may relay the upstream's own `5xx`.
**Status:** empirical finding — read out of the shipped opencode binary
(`~/.opencode/bin/opencode`, a compiled bun bundle; `@opencode-ai/plugin`
1.15.4). Minified identifiers below are as found.

## What opencode does

**Retry decision.** A failure is retried when it is retryable *or* carries a 5xx
status — the 5xx arm is a second, independent path, not a restatement:

```js
if (!A.data.isRetryable && !(E !== void 0 && E >= 500)) return;   // E = statusCode
```

`isRetryable` is the AI SDK's own predicate, which already admits 5xx:

```js
isRetryable: N = Q != null && (Q === 408 || Q === 409 || Q === 429 || Q >= 500 …)
```

**Delay selection.** Status-agnostic — it reads the headers off whatever error it
was handed, never gating on the code:

```js
let e = r["retry-after-ms"];  // preferred, parsed as float ms
let B = r["retry-after"];     // then delta-seconds, then HTTP-date
// no header → exponential backoff, capped
```

So `503` + `Retry-After: 5` and `429` + `Retry-After: 5` get identical treatment.
Note `retry-after-ms` is consulted **first**; understudy does not emit it.

## Implication for the code

- **understudy need not convert a busy/retryable upstream failure to `429`.**
  Relaying the upstream's own `503` with a synthesized `Retry-After` produces the
  same client behavior, and is the honest status — `503` is defined for exactly
  this (temporary overload), where `429` would assert the client sent too many
  requests and `502` would blame the gateway.
- This is what DESIGN §Understudy *Synthesized backoff* already prescribes
  ("synthesizes a `Retry-After` and injects it while **preserving the retryable
  status**"); the finding removes the one reason to depart from it.
- Retiring the reject path is **not** implied. That exists because opencode
  honors a *long* `Retry-After` essentially unboundedly, which is orthogonal to
  which status carries it.

## The agent turn does not run this loop (2026-08-14)

All of the above describes what the retry loop does **when it runs**. On the path
understudy actually serves, it does not. The bundle's `streamText` call passes
`maxRetries: a.retries ?? 0`, and the agent turn supplies no `retries`; the only
caller that does is title generation (`retries: 2`). The SDK's own
`{maxRetries: $}` helper defaults to 2 **only when the value is null**, so an
explicit `0` yields one attempt, and neither the retryable predicate nor the
delay selection above is ever consulted.

So the finding's conclusion — `503` + `Retry-After` behaves like `429` +
`Retry-After` — still holds, but degenerately: on an agent turn neither is
retried. Nothing understudy injects reaches a reader there.
[[fail-over-from-a-bare-429]] measures what that costs.

## Caveat

This is read from one shipped build, not from opencode's source or a documented
contract. The `>= 500` arm and the AI SDK predicate are independent, so a change
to either alone would not flip the conclusion — but a deliberate narrowing of
both would, and nothing here is a promise opencode has made.
