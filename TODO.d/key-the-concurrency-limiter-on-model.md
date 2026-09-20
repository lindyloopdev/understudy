# Key the concurrency limiter on model

**Tag:** understudy / ratelimit / concurrency

**Design:** [DESIGN.md §Concurrency & Rate Limiting](../DESIGN.md#concurrency-rate-limiting)
— the learned cap and the bare 429 as its measurement;
[notes/2026-07-13-health-identity-granularity.md](../notes/2026-07-13-health-identity-granularity.md)
— why health already keys on `url + key + model`.

The limiter keys on the account (`canonicalUpstreamKey`: `url + key`), so a
bare 429 on one model shrinks the in-flight cap for every model on that
account. Providers scope failures per model — Anthropic's per-model limits,
and per-model 4xx/5xx when one model misbehaves — and health already honors
that; only the bare 429's route into the limiter still coalesces siblings.

## Work

- Decide the key: `url + key + model` makes a model's bare 429 shrink only its
  own cap, but under a limit that really is account-wide the per-model caps
  sum past it and each model pays its own rejections to learn that. No
  provider is yet observed either way; gather that before choosing.
- Tests: a bare 429 on one model leaves a sibling model's cap unchanged.
