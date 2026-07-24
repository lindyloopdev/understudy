# Availability failover: walk a candidate list, pin per session

**Tag:** understudy / fallback / ha

**Design:** [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy).

understudy's **availability execution** layer: given an orchestrator-composed
ordered candidate list, decide each request's disposition and pin a target per
session. The **selection** of the list (cost/quality) is the orchestrator's —
[[understudy-model-groups]]. The disposition ladder and the affinity/admission
frame are settled in [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy);
this item tracks the remaining build.

Built today: `[understudy.models.<name>]` resolves to `LogicalModel{ Targets
[]Target }` (validated at `Resolve`); `pickTarget` picks the first target not
failing past `defaultFailoverThreshold` (15s), with half-open re-probes, advancing
on a *classified* outcome (`classifyLimit`/`isFatalUpstream`) keyed on the canonical
upstream account+model (`healthKey`), so targets on one account+model share a health
entry across backend names. **Quota-class-aware demotion** is built too: a per-day 429
benches its target to the daily reset, a per-minute one honors its short delay.
The failover today is **cross-request** — the request that *triggers* a demotion
still returns the error; only a later request is routed around.

## Build path (staged)

1. **Within-request failover (near-term).** Serve the *triggering* request from
   the next healthy target instead of returning the error, so a sustained-rate-
   limited or per-day-benched target is invisible to the client. Triggers on the
   `sustainedRate` classification (a 502 past the blip threshold too), **not** a
   transient throttle; within-request disposition is orthogonal to demotion.
   Requires buffering the request body (replayable across targets) and re-running
   the per-target model rewrite. The walk must **terminate and surface** the 429
   (or reject) once every target has been tried — never loop back onto an
   exhausted list. This is the fix for the per-day 429 storm.
2. **Wait-in-place (staged).** Absorb a *short* throttle in understudy — sleep and
   retry the same target — instead of relaying it, keeping the preferred model's
   cache/coherence. `T_wait` is empirical (held-connection cost), bounded by the
   client timeout.
3. **Affinity + admission (staged, spike-first).** Hold-open makes understudy an
   admission scheduler: per-request wait budget by coherence, priority to live
   sessions, shed the rest. Gated on a payload-inferred **session key** (opencode
   passes none — feasibility-spike first) and a **capacity model** (RPM budget +
   concurrency slots).

The deeper mechanics below — the explicit **session-pin** state and the **latency
gate** — underlie steps 2–3.

## Session pin (the whole within-beat story)

Today `pickTarget` runs per-request and leans on the stable cost order + persisted
per-target health for *de-facto* stickiness; there is no explicit incumbent
pointer. The remaining work is the real session-binding state — two states, one
transition:

```
created → [orchestrator cost-min select] → pinned ⇄ (serve turns)
                                              │
   pin failing past failover threshold (or deadline) → orphaned
                                              │
        re-walk list from top, first available → pinned
                                              │
                    list exhausted (all down) → fail fast
                                              │
          lindyd defers / re-runs as a NEW beat → (fresh cost-min)
```

The policy — stick-while-healthy, "fails to serve" = threshold-streak or blown
liveness deadline, re-walk-from-the-top on a forced re-pin, and ephemeral
per-session (not cross-beat) incumbent state — is settled in the doc's *Session
target binding* / *Affinity and admission*. The work here is the explicit
incumbent-pointer state, not the policy.

## Invariants the remaining work must keep

The flat-first cut honored three commitments; the session-pin and latency-gate work
must not break them:

1. **A target is `(backend, model)`, and the list is N-of-them** — never a
   "primary + one backup" pair. N unblocks cost-tiers and the walk.
2. **Advance on a *classified outcome*, not an inlined `if status >= 500`.** The
   walk asks "did this attempt fail to serve?"; a classifier answers. Deadline and
   rate-limit are later inputs to the *same* classifier — and it should be the
   *same* seam that classifies the rate-limit reject
   ([[understudy-ratelimit-firewall]]), not a second status inspection.
3. **Resolution (order the candidates) stays distinct from execution (walk).** The
   walk consumes an already-ordered list and never re-derives order; ordering
   policies then enrich resolution as a signature-widening, not a walk rewrite.

Anti-patterns that paint the corner: primary/secondary pair; status-check fused
into the proxy loop; config-order derived inside the walk.

## The latency gate (local-LLM viability)

A slow-but-alive target (deepseek yesterday; a backed-up local LLM) should count
as unavailable. Two distinct latency signals — do not conflate:

- **Expected/historical latency** (a long-run average per target) → feeds the
  orchestrator's cost *order*. Stable; fine when minutes old.
- **Instantaneous load** ("queue right now") → gates *usability* at request time.
  Volatile on a seconds timescale; **never baked** into the list — always
  discovered live.

Freshness tiers for the instantaneous signal (richest available wins, deadline
always underneath):

```
queue-depth projection (local server exposes it, configured)  ← causal, fresh even when idle
   ↓ else
historical TTFT EWMA at understudy                            ← lagging; stale for an idle backend
   ↓ else / and always underneath
liveness deadline                                            ← reactive, measured on the real attempt, never stale
```

- **Deadline is the floor** and is load-bearing for local: an idle backend's TTFT
  is stale even at understudy (nobody measured it while the session sat
  elsewhere), so the safe default is an **optimistic re-pin corrected by the
  deadline** — try the cheapest-in-order, let a bad guess trip the deadline and
  advance. Bounded cost: one wasted interval.
- **Queue-depth projection** is the predictive upgrade — `queue_depth ×
  understudy's observed avg job-duration` projects time-to-start causally, fresh
  without a probe. It is a per-target, beat-agnostic health signal (understudy's
  substrate). **Caveat:** queue depth is *shared* — routing N sessions on one
  reading stampedes the queue; projections must count in-flight routes and jitter,
  sharing the per-host-state substrate with
  [[understudy-adaptive-coordinated-backoff]].
- The **gate threshold** is not a per-target constant (`max_ttft` was wrong): the
  tolerable wait derives from the cost of the *cheapest currently-available
  fallback* (dear fallback → wait longer), so it is composed by the orchestrator
  (price + `time_value`) and handed down, compared against understudy's live
  measurement.

## Non-goals

- **Asymmetric per-edge triggers** (fall from X to Y on rate-limit but to Z on
  hard-down) — a routing graph, not a list. YAGNI.
- **Full dynamic cost re-sort / permutation** (candidates whose cost order
  arbitrarily permutes at runtime). Realistic cases are monotonic (cheap-when-fast,
  drop-out-when-slow), which the availability gate covers.
- **Live cross-tier escalation inside understudy.** Escalating to a pricier tier
  when the cheap set is exhausted is the orchestrator's cost decision, made between
  attempts; understudy fails fast and lets lindyd escalate/defer.
