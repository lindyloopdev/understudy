# Health/failover identity granularity: key by canonical url+key+model

Point-in-time analysis (2026-07-13) behind the decision to re-key per-target
failover health from the backend *name* to the canonical upstream identity.

## Current state

- **Concurrency limiter** keys by `(canonical base URL + API key)` — the upstream
  *account*. Canonicalized (trailing slash folded) as of the
  `canonicalUpstreamKey` helper.
- **Failover health** keys by `Target.identity()` = `backend + "/" + model`
  (target.go) — i.e. the operator-chosen backend *name* plus the model. Demotion
  fires on `chosen` (a `backend/model`) for: sustained-rate 429, signal-less
  429-when-alone, rate-limit-with-Retry-After, and fatal upstream errors
  (understudy.go ~1252).

## The two failure dimensions health lumps together

1. **Account-wide** — a 429 rate limit, an auth failure, the account being down.
   These are properties of the *account* (`url + key`): every model on that
   account is affected. Today a 429 on `X/glm` demotes only `X/glm`; a sibling
   `X/air` keeps routing to the throttled account until it independently 429s.
2. **Per-model** — a model decommissioned or overloaded (a fatal error on one
   model). A property of `(account + model)`: it must NOT demote sibling models on
   the same account.

Keying on `(name/model)` is wrong on both axes: it uses an arbitrary config
*label* instead of the real upstream, and it treats account-wide 429s as
per-model.

## Decision

Health identity = **canonical `url + key + model`**.

- `model` stays in the key — dropping to `url + key` alone would over-coalesce,
  letting a per-model fatal error demote sibling models (dimension 2).
- The real fix is replacing the backend *name* with the canonical *account*
  (same `canonicalUpstreamKey` normalization the limiter uses), so two configs
  naming one account+model track one health entry.

## Why it is NOT built now (deferred to the daemon, slice 3)

- In a single process, a backend name maps bijectively to one `(url+key)`, so
  `(name/model)` and `(canonical url+key+model)` are functionally identical —
  there is no present in-process behavior to change, and no non-degenerate RED
  test distinguishes them. The only construct that diverges is two backend
  *names* resolving to the same `(url+key)` — a pointless single-config failover
  list, but the normal case **cross-tenant** in the shared daemon.
- So the payoff is cross-tenant, coupling this to the shared-daemon work. Same
  premature-work shape as idle eviction.
- The account-wide part of a 429 is *already* handled separately: the
  (account-keyed) limiter shrinks the whole account's cap on a 429. Failover
  health is the distinct "route this logical model to a *different* account"
  response, which is legitimately per-target — so the account-wide propagation
  gap is narrower than it first appears.

Lands with slice 3, alongside `Registry` relocation and eviction.
