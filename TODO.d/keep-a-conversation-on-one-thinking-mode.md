# Keep a conversation on one thinking mode

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Switching cost … is
set by conversation position: a *subsequent request* forfeits the pinned target's warm
cache and mid-conversation coherence", and the staged affinity work below it, whose
session identity a fix would key on.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — demotion and half-open
re-admission, which are what move a conversation between targets mid-flight.

`disablesThinking` is per-target, so a pool may hold targets that disagree about
thinking, and nothing binds a conversation to the one that started it. A history built
under one mode can be sent to a target expecting the other, and a provider that
requires `reasoning_content` rejects the whole history rather than degrading.

Measured 2026-08-11, from lindy's step records: a `review-standard` beat was served
its first turn by `deepseek/deepseek-v4-flash`, its next seventeen by
`z-ai/glm-4.7?thinking=false`, then died on

    400 The `reasoning_content` in the thinking mode must be passed back to the API.

A later run of seventeen beats crossed no targets and saw no such failure. The
crossing mechanism fires regularly: z-ai answered 11 `429`s in five minutes, and each
one falls the walk through to `deepseek`, last in that pool.

**Settle the cause before building.** The message says a `reasoning_content` the API
emitted was not returned — not that every assistant turn needs one. Three candidates:

- The crossing, as above.
- The caller never echoes `reasoning_content`, and understudy only supplied the
  occasion. One probe covers both: does a two-turn single-target DeepSeek conversation
  through the same caller succeed? If it fails, delete this entry.
- `?thinking=false` might be a no-op against z-ai — unverified there, but measured as
  one against kronk in [[understudy-thinking-disable-llamacpp]], which injects the same
  Anthropic shape. If z-ai ignores it too, GLM was thinking throughout and is the one
  complaining. Probe: count reasoning tokens with `thinking:{"type":"disabled"}` set.

If the crossing is the cause, three fixes, cheapest first:

- **Match the target to the history.** understudy already normalizes a body per
  target; decide the mode from what the history *is*. Needs no session identity, so it
  is the only one testable per request — but it overrides a per-model economic choice,
  and covers only the direction where the field is missing.
- **Bind a conversation to its target.** The session identity already staged for
  affinity, arriving earlier and for a harder reason.
- **Strip on the way out.** Costs the byte-faithfulness the rewrite path is built
  around, and is needed only if a surplus `reasoning_content` also offends.

A pool whose targets agree about thinking would stop the symptom and is not the fix:
thinking is a per-target economic call — enable where it is cheap, disable on a model
strong enough without it — so a mixed pool is what a pool is for.

Whichever lands, note what can guard it: a per-request rule is testable today, a rule
about conversations is not, because understudy cannot tell one conversation from two.
