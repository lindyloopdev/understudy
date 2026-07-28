# Synthesized backoff + coordination: supersede opencode's flat 30s retry

**Tag:** understudy / fallback / ha / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

The **settled behavior** — understudy synthesizes an exponential, jittered,
reset-on-success `Retry-After` for a retryable failure that carries none, capped
at the rate-limit-reject threshold — is stated in
[DESIGN.md §Understudy](../DESIGN.md#understudy). Sequenced **after** the stateless
rate-limit reject ([[understudy-ratelimit-firewall]]); part of the per-host
availability layer in [[understudy-scope]] (§failover + circuit-breaker).

## Remaining work

- **Graduated injected backoff.** understudy still injects only a **fixed** 60s
  `synthesizedRateLimitRetryAfter`; grow the injected interval exponentially from
  `failingSince` (5 → 10 → 20 → 40 …, jittered, reset-on-success) — the Mechanism
  below.
- **Terminal-threshold trip on a header-less streak.** The terminal `400` reject
  fires only on an *advertised* `Retry-After` past the 2m cap; a header-less
  5xx/429 storm that exhausts every target just returns the last one instead of
  escalating. Trip into the 400 once a target's streak crosses the terminal
  threshold with nowhere to fail over.
- **Pre-header stall gate — tune constants and add the coherence budget.** The
  gate demotes-and-replays on a stall using provisional `headerStallGate` (20s)
  and `synthesizedStallBackoff` (30s), with a **uniform** budget for every
  request. Settle both empirically; then add the **coherence-sized wait budget**
  that separates a first request (cheap replay, ~0 budget) from a coherent
  subsequent one (switching forfeits cache — a larger budget), likely inferred
  from request-body size. Later, refine the synthesized backoff from ollama
  queue length.
- **Cross-session coordination** (the aggregate-health value-add). The health map
  + limiters are process-local; pushing every session's interval up together as a
  host degrades and pulling down on recovery needs the shared daemon
  ([[understudy-shared-daemon-subserver]]).

Terminal status stays **`400` + typed envelope** (lindy classifies on the envelope
`type`, not status; 424 buys only semantic tidiness — not worth changing shipped
behavior). The circuit breaker is **this feature's binary degenerate** — same
per-backend health state, tripping straight into the reject instead of dialling
the interval up first.

**Unresolved (v1 rung):** the **graduated** injected-backoff assumes opencode
honors an understudy-*injected* `Retry-After` on a retryable response — still
unconfirmed (the 2026-07-03 repro drove only the plain 502 storm, not injection).
The header-less terminal trip needs no such assumption (it reuses the shipped
reject and the existing streak/`pickTarget` redirect), so it can land first. The lindy-side [[review-beat-idle-timeout]] is a coarse
stopgap this supersedes.

## Mechanism

For a retryable failure that carries **no upstream `Retry-After`** (a 429 with no
header, or a 5xx), opencode self-caps its backoff at a flat
`RETRY_MAX_DELAY_NO_HEADERS = 30s` and retries with no ceiling. understudy
**synthesizes** its own `Retry-After` and injects it on the response (keeping the
retryable status), so opencode sleeps *understudy's* interval instead of its flat
30s. It tracks the per-`(backend, model)` failing streak (`failingSince`, a
timestamp — not a count) and grows the interval exponentially (5 → 10 → 20 → 40 …),
resetting on success.

## Why understudy and not opencode

understudy is the in-path choke point across **all** sessions, so it sees the
aggregate health of a host. A single opencode flailing at flat 30s is blind to
whether `z.ai` is melting for everyone. understudy can **coordinate** backoff
across sessions — push every session's interval up together as a host degrades,
pull it down as it recovers. No individual opencode can do this. It is the
continuous (graduated) form of the binary circuit-breaker — same per-host health
state, dialled instead of tripped.

## Where it slots in (per-host decision tree, retryable failure)

**Retry the current target first** — a single 5xx is a blip, not a failure — and
fail over only once it has been failing *past a threshold*. On a retryable failure
from the current target:

1. **Back off in place.** Record/continue the target's failing streak
   (`failingSince`, per `(backend, model)` — see Granularity — set on the first
   failure, cleared on any success) and inject a coordinated, jittered exponential
   `Retry-After` (keep the retryable status) so opencode retries the *same* target
   at a sane pace.
2. **Failing longer than the FAILOVER threshold AND an alternate exists** →
   **redirect** to the next target (it becomes current).
3. **No alternate (or all exhausted), failing longer than the TERMINAL
   threshold** → **rewrite to terminal 400** (the reject's terminal case).

**Thresholds are durations, not failure counts.** understudy serves many clients,
so a count-based trip fires at volume-dependent wall-clock intervals (10 clients
reach N failures in milliseconds; 1 client takes seconds); a duration is
volume-independent — "this target has been failing for 15s" regardless of request
rate. The **failover** threshold (earlier, ≈15s) leaves a failing target for a
configured alternate; the **terminal** threshold (later, ≈2m — the backoff cap)
hard-fails when there is nowhere to go. Both are hard-coded for now; when cost
exists they become **cost-derived** — an equal-cost alternate → ~0 (fail over at
once, the old "failover first"), a pricier alternate → larger (retry the cheaper
target longer). Immediate-vs-retry-first was never a fixed choice: it is the two
ends of this one cost-derived threshold.

## Granularity: per-`(backend, model)`

Track `failingSince` per **target** `(backend, model)`, not per backend, and never
inferred wider from error-body heuristics. A failure is a fact about the one
target you called; attributing it to a whole backend is an inference the API can't
reliably support (is a 5xx model-specific or backend-wide?). The asymmetry decides
it: per-model when the outage is backend-wide → each model re-learns independently
(redundant, still correct); per-backend when the failure is model-specific → a
healthy sibling model is penalized (false positive). Finer granularity can only
under-attribute, never punish the innocent. (The one clean, body-free signal — a
*connection* failure vs. an *HTTP* 5xx — could later attribute connection failures
backend-wide to speed multi-model failover; an optimization, not the first cut.)

## Required guards (or it backfires)

- **Cap = the terminal threshold.** Without a ceiling the exponential climb
  (40 → 80 → 160 …) rides opencode's *no-ceiling* retry loop straight back into
  the multi-day-hang territory the reject exists to kill. The interval climbs only
  until the streak crosses the **failover** threshold (hand off to an alternate)
  or, absent one, the **terminal** threshold (hand off to the 400). The terminal
  threshold doubles as the backoff cap.
- **Jitter.** Coordinated backoff is exactly what synchronizes sessions; without
  per-session jitter they wake in lockstep and thundering-herd the recovering host.
- **Reset on success**, so a recovered host snaps back to no-backoff.

## Charter caveat

This crosses understudy from "relay the upstream's headers" to **synthesize its
own control signal** — from availability *proxy* toward active *traffic shaper*.
Still availability, not quality (so [[understudy-scope]]'s dividing line holds),
but "pure proxy" weakens to "controller." State the expansion explicitly when
building.

## Relationship to the other stateful ideas

Shares one per-host health-state substrate with failover (go elsewhere) and the
circuit breaker (the binary degenerate of this backoff). Together they are the
post-v1 **per-host availability layer**, all capped by the same threshold the
stateless reject uses.
