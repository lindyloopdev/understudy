# [BUG] A `backend/model` reference drops its query overrides

**Tag:** understudy / config

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the per-target
request-body overrides a target carries, and the two ways a client can name what
it wants (a declared logical model, or a `backend/model` reference).

A target written in config parses its `?key=value` overrides; the same string
sent as a request's model name does not. `chatCompletions`'s rewrite callback
resolves a `backend/model` reference with a bare `strings.Cut(model, "/")`
(`understudy.go:1563`) and forwards everything after the slash as the upstream
model name. `Target.UnmarshalText` (`target.go:29`) — the one place that knows a
target reference may carry a query — is never reached, so `?thinking=false`
travels to the provider as part of the model code.

## Observed

```
$ lindy review --model "z-ai/glm-4.7?thinking=false" review-spelling
… err="opencode session error: APIError: upstream returned status 400:
   Unknown Model, please check the model code."
```

z.ai is being asked for a model literally named `glm-4.7?thinking=false`. The
identical string as a `targets` entry serves fine, and `--model z-ai/glm-4.7`
(no query) serves fine — only the combination fails.

The effect is not just a rejected request: where a provider *accepts* the
unknown-parameter form rather than 400-ing, the override is silently not applied
and the caller gets thinking-on output while believing it asked for thinking-off.

## Solution

Parse the reference through `Target.UnmarshalText` rather than re-splitting it,
so one parser answers "what is this reference" for both config and request paths
and the overrides apply identically. `validate()`'s rules (thinking must be a
strict boolean; `thinking=true` reserved — [[understudy-thinking-disable]]) then
cover request-named targets too, which they currently do not.

This lands in the code region [[parse-model-reference-outside-the-rewrite]]
reshapes. That refactor wants name-interpretation extracted as a pure function;
this bug is a defect *in* that interpretation. Sequencing either way works —
fix-then-refactor, or fold the fix into the extracted parser — but they should
not be written twice.

## Workaround

Declare a logical model in `lindy.toml` whose single target carries the query,
and name that instead. Used by lindy's cost-reduction conformance sweep
(`probe-*` groups) purely to work around this.
