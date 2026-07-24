# Scope: an availability proxy, not a model picker

**Tag:** understudy

**Design:** [DESIGN.md §understudy](../DESIGN.md#understudy).

**Settled architecture** — the availability/quality dividing line, the model
addressing grammar (logical model vs direct target), `default` resolution,
rate-limit reject, credential brokering, the embeddable proxy — is in
[DESIGN.md §Understudy](../DESIGN.md#understudy) and
[§LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy). This
doc is the **unsettled** remainder: how a logical model's membership is composed
(the quality half — [[understudy-model-groups]]), plus the open/phasing items.
The rate-limit reject detail is [[understudy-ratelimit-firewall]].

## Provenance reporting

understudy logs *which target actually served* a request server-side
(`model_upstream` beside `model_requested`), so that when a fallback fires, quality
accounting can attribute the outcome to what really ran. Remaining: surface the
served target back to the client/beat (a fallback's outcome is not yet attributed to
the beat — `BeatStart.Model` carries only the requested logical name), and deliver
the unknown-logical-model WARN to the config owner (per-tenant routing +
request-originator/CLI surfacing). Detail: [[understudy-provenance-reporting]].

## Quality lives in the list's composition, not understudy's execution

Cross-provider failover is almost always a *model substitution* (providers rarely
share models), and a substitution is a quality event. understudy stays
quality-agnostic anyway, because it **executes** a list of concrete targets — it
never **picks** one. The quality judgment was made by whoever **composed** the list:

- **within a declared-equivalent set**, failover is quality-neutral *by declaration*
  (lindy's calibrated indifference class, or a standalone operator's accepted order);
- **across quality** (to a genuinely better/worse model) is a quality decision — the
  orchestrator's escalate-vs-defer, fired when the equivalence set is exhausted.

So the equivalence-class boundary is the line: within-class availability is
understudy; cross-class quality is the orchestrator.

## One proxy, two worlds

- **Standalone** (opencode or any OpenAI-compatible client points at understudy): the
  operator composes preference lists (`opencode-go → z.ai → anthropic`;
  `local → cloud`) by hand, and understudy "just works" — credit-exhaustion and
  local-overload fall through automatically, no orchestrator required.
- **Inside lindy**: lindy composes the lists from calibration + preference and hands
  understudy a quality-equivalent set (or a single target), keeping cross-class
  escalation for itself.

## Config is preference (permanent), not quality (decaying)

understudy's config expresses **preferences/policy** — which providers exist, cost
ceilings, privacy/egress rules, local-first, failover order. These are *oughts*, not
*ises*: there is no empirical ground truth for calibration to overrule, so gut-feel
config here is authoritative and **permanent**. Contrast the orchestrator's
*quality* estimates, which are facts calibration learns — there, gut-feel is a
decaying prior ([[understudy-model-groups]]). A failover list is mostly preference
(its **order**), with a quality-**membership** component ("is this an acceptable
substitute?") that calibration *can* later inform but never owns.

## Open / phasing

- **Multi-target availability failover + circuit-breaker** are built: config-defined
  duration-threshold failover across an ordered target list (`pickTarget`), with the
  per-backend breaker as demotion + half-open re-probe on recovery
  ([[understudy-fallback]]). The remaining availability work — graduated backoff,
  header-less terminal trip, cross-session coordination — is
  [[understudy-adaptive-coordinated-backoff]].
- **Addressing vs. token-scoping.** Does understudy keep trivial `<backend>/<model>`
  addressing (parse prefix → pick credential, no decision) or become token-scoped
  (one backend per token, zero parsing)? Decides whether the prefix-parse and the
  model-rewrite prefix-buffer ([[understudy-stream-model-rewrite]]) stay or retire.
- **Provider-translation (deferred).** Tool-call format normalization across
  providers; stripping provider-specific response artifacts (thinking blocks,
  reasoning tokens); documenting mid-session hopping. v2 concerns; the current
  proxy path forwards provider shapes as-is.

## Note: token-usage observability (future, cross-cutting)

Not designed yet — just keep it in mind while designing the other bits, because it
touches several. As the single in-path choke point across all keyed providers,
understudy is the natural place to **meter token usage** (the `usage` field is already
in every response it relays). Likely shape: understudy records *tokens* (the fact,
tagged by backend/model and the token's resolved scene/tenant) and emits usage events;
**pricing** (tokens × price — a churning config table) is applied *downstream* by the
embedder, not in understudy. It would feed three things, so the other designs should
leave room for it: cross-provider **cost dashboards / per-tenant metering**, the
orchestrator's **cost-aware selection** (this is where real cost data originates), and
understudy's own **credit-exhaustion failover** (meter against a budget → trip the
circuit-breaker before the provider 429s). Composes with the orchestrator's per-beat
provenance (per-request usage joined to beats on the token→scene identity). Covers
understudy-routed traffic only; configless free-tier bypasses it. Post-v1, additive.
