# Generalize per-target body overrides to JSON-valued params

**Tag:** understudy / normalization / cost

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Per-target
request-body normalization": overrides ride inline on the target reference,
decoding is purely structural, and they apply only to the pinned target.
Reasoning/rejections:
[notes/2026-07-05-understudy-per-target-body-normalization.md](../notes/2026-07-05-understudy-per-target-body-normalization.md).

The override axis is one bit wide and hardcoded: `thinking` is the only key any
consumer reads (`Target.disablesThinking`, its one caller in `understudy.go`), so
every other body parameter a target might need is unreachable. Adding each one as
its own special case does not scale with the rate models ship knobs.

Make the value grammar JSON and forward any key generically:

```
z-ai/glm-5.2?reasoning_effort="high"
z-ai/glm-5.2?thinking={"type":"disabled"}&temperature=0.2
```

`json.Unmarshal` each value into the forwarded body under its key. Types are
explicit rather than inferred from a bare string — `"high"` is a string, `0.2` a
number — and nesting is expressible, which a flat `key=value` is not: `thinking`
already needs `{"type":"disabled"}` rather than a scalar. A value that is not
valid JSON is a domain-rule violation, refused wherever a reference is accepted
(`Config.Resolve`, and `400` when a request names the reference directly), which
is where the design already puts value semantics.

**Understudy validates shape, never vocabulary.** Whether a model accepts
`reasoning_effort` is the upstream's to answer, and it does, by rejecting the
request and naming the parameter. An allowlist here would be stale at the next
model release and would have to be taught every knob; forwarding costs nothing
and keeps the authority where the knowledge is. The one thing config must not
reach is the keys understudy owns — `model`, `messages`, `stream` — which is an
assumption about understudy's own contract, stable across model releases, unlike
an assumption about any model's parameters.

**This keeps the ignore-unrecognized rule by making it vacuous.** §Understudy
takes unknown params as ignored, not rejected, so a new override can roll out
across a fleet ahead of the binary that understands it. Generic forwarding serves
that goal strictly better: a binary that has never heard of a param forwards it
instead of dropping it, so the override works the moment the *upstream* supports
it, with no binary rollout at all. Nothing is silently discarded because no key is
unrecognized.

Retires two special cases rather than adding a third. `?thinking=false` becomes
sugar for `thinking={"type":"disabled"}` — keep it or migrate the four targets in
lindy's `lindy.toml` and drop it. And [[understudy-thinking-disable]]'s tri-state
falls out for free as `thinking={"type":"enabled"}`, so the reserved-and-erroring
`thinking=true` and its inline `TODO` marker in `Target` both go.

**Driver.** lindy's `frontier` tier (`z-ai/glm-5.2`, no overrides) sets no
`reasoning_effort`, and GLM-5.2's chat template resolves the absent value to
`max` — its deepest and most expensive setting — while `high` is the documented
latency-balanced level and is opt-in
([vLLM recipe](https://recipes.vllm.ai/zai-org/GLM-5.2)). Today that middle
setting is unreachable: `z-ai/glm-5.2?reasoning_effort=high` parses, validates,
and round-trips through daemon registration intact, then is silently dropped. So
a latency sweep over that tier can compare only max against thinking-off, and the
setting most likely to be right cannot be tested.
