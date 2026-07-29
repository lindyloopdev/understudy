# Skip an unusable backend instead of failing the request

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Least degradation:
a backend understudy cannot use costs that backend, not the request", and the four
paragraphs after it (skipped-not-demoted, the reason travels, operator vs caller,
two endpoints).

A backend that reached understudy without a base URL fails **every** request
against that configuration. The auth middleware (`understudy.go:1009-1015`) loops
over all backends before any handler runs and 500s on the first one missing a base
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

`resolveBackend` already decides routability, and its doc comment currently
disclaims field validation as "owned by the auth boundary". That ownership moves
here: it becomes the one place a backend is judged usable, for every selection
site.

## Build path

Three steps. Only the second and third change behavior.

1. **`resolveBackend` reports why, not whether** — a strict refactor. Return
   `(selection, error)`: nil for routable, otherwise the reason (unregistered
   `provider_type`, missing base URL). Callers translate the reason back into the
   message they emit today, so nothing observable changes; zero test diff.

   This step fixes nothing on its own, and staging it separately is the point —
   the signature change is mechanical and the behavior change is not. There is no
   observable delta to demonstrate because both unusability cases are already
   masked: an unregistered `provider_type` is *already* skipped by
   `firstCandidateBackend`, and a missing base URL never reaches a handler while
   the middleware pre-flight stands.

2. **Skip unusable backends at selection, and retire the blanket pre-flight.**
   Drop the middleware loop, so a request is never failed for a backend it does
   not resolve to. This is the fix: a `default` request against `{broken, good}`
   now reaches `good`, and a request resolving to `x/a` no longer dies because
   `y` is malformed.

   Two things must land with it, not after.

   - **The nil base URL must be caught before `canonicalUpstreamKey`**
     (`understudy.go:1608`), which dereferences `*baseURL` unconditionally. The
     middleware check is the only thing preventing that panic today, so removing
     it without the selection-time check trades a 500 for a recovered panic.
   - **The chat path must report the real reason.** Once a malformed backend is no
     longer rejected up front, a direct request for `alpha/gpt-4` reaches
     `model references unknown backend "alpha"` (`understudy.go:1575`) — a
     falsehood the design forbids, since `alpha` *is* configured. So fold
     `errNoBackendConfigured` into one error naming the unresolvable model and
     carrying the skip reason where there is one, and distinguish *unknown* from
     *known but unusable*. Deferring this ships a knowingly false error message.

   Report the skip to the operator on `s.logger` at ERROR, deduplicated per
   condition the way `noteBackendDown` (`understudy.go:599-607`) dedupes a streak
   — `Validate` runs on every request, so an undeduplicated log floods.

3. **`/v1/models` answers its own question.** Skip unusable backends and stop
   aborting the listing when a catalog fetch fails (`understudy.go:1300`), making
   the endpoint effectively non-failing. Independent of step 2: the chat path is
   what couples to the skip, the listing does not.

## Test surface

Step 1 changes none of it — that is the check on whether it stayed a refactor.

Step 2 rewrites the chat-path expectations: the `LogRecord` error-attr test
pinning `no backend configured` (`understudy.go:1565` is its other consumer), and
whatever asserts `model references unknown backend` for a backend that is declared
rather than absent.

Step 3 turns three `/v1/models` cases from 500 into 200 — "should return 500 when
no backend configured", the sole nil-`BaseURL` case, and "should return 500 when
any backend has a nil config even if another is usable". The last one pins the
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
