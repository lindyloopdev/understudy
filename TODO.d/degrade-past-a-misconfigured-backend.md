# Skip an unusable backend instead of failing the request

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Least degradation:
a backend understudy cannot use costs that backend, not the request", and the four
paragraphs after it (skipped-not-demoted, the reason travels, operator vs caller,
two endpoints).

A backend that reached understudy without a base URL fails **every** request
against that configuration. `authMiddleware` loops over all backends before any
handler runs and 500s on the first one missing a base
URL — deliberately, "regardless of map iteration order", to keep the error
deterministic. So given a logical model with targets `[x/a, y/b, z/a]` and an
unusable `y`, a request resolving to `x/a` dies at the trust boundary having never
needed `y`.

This is not reachable from a TOML config: `BackendSpec.BaseURL` carries
`validate:"required,url"` and `Resolve` applies `Validate` before reading anything,
so the document fails to load and the operator is told at config time. It is
reachable when a `TokenValidator` returns a `BackendConfig` it did not build
through `Config.Resolve` — the embedded and multi-tenant case, where the config
arrives per-token and there is no load-time gate.

`resolveBackend` decides routability and reports why a backend is not routable;
its doc comment still disclaims field validation as "owned by the auth boundary".
That ownership moves here — it becomes the one place a backend is judged usable,
for every selection site — which means giving it the missing-base-URL reason.

## Build path

Two steps, either shippable alone.

**Skip unusable backends at selection, and retire the blanket pre-flight.** Drop
the middleware loop, so a request is never failed for a backend it does not
resolve to. This is the fix: a logical model with targets `[x/a, y/b, z/a]` serves
from `x/a` while `y` is unusable, rather than failing at the trust boundary. A
logical model whose targets span two backends is the vehicle for the test.

Two things must land with it, not after:

- **The nil base URL must be caught before `canonicalUpstreamKey`**, which
  `chatCompletions` reaches through `upstreamLimiter` and which dereferences
  `*baseURL` unconditionally. The middleware check is the only thing preventing
  that panic, so removing it without the selection-time check trades a 500 for a
  recovered panic.
- **The chat path must report the real reason.** Once a malformed backend is no
  longer rejected up front, a direct request for `alpha/gpt-4` reaches
  `model references unknown backend "alpha"` — a falsehood the design forbids,
  since `alpha` *is* configured. So distinguish *unknown* from *known but
  unusable*, and carry the skip reason into the error. Deferring this ships a
  knowingly false error message.

Report the skip to the operator on `s.logger` at ERROR, deduplicated per condition
the way `noteBackendDown` dedupes a streak — `Validate` runs on every request, so
an undeduplicated log floods.

Test surface: whatever asserts `model references unknown backend`, or a 500 from
the retired pre-flight, for a backend that is declared rather than absent.

**`/v1/models` answers its own question.** Skip unusable backends in the listing
too, so a backend understudy cannot use contributes nothing instead of deciding
the response. Independent of the skip work: the chat path is what couples to it,
the listing does not.

Test surface: three `/v1/models` cases turn from 500 into 200 — "should return 500
when no backend configured", the sole nil-`BaseURL` case, and "should return 500
when any backend has a nil config even if another is usable". The last pins the
defect itself, so it inverts rather than merely changing status.

## Out of scope

- **Failover for a target unusable at runtime.** A refused credential or an
  unreachable host still demotes and is walked past; only *static* unusability is
  a skip. See [[fail-over-in-place-from-a-demoted-target]].
- **An unset credential under `auth = "auto"`.** The document is correct and the
  world is not supplying a key, which seeds health state rather than skipping —
  see [[auth-requirement-and-key-env-source]] and [[resolve-validate-split]].
- **Exposing the registered provider set** so a consumer can pre-check
  routability itself. Deliberately deferred until one needs it; a consumer can
  already enforce any rule it *can* see from its own `TokenValidator`.
