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

The trigger, measured against both upstreams 2026-08-12: DeepSeek in thinking mode
rejects an assistant turn that carries `tool_calls` without `reasoning_content`, and
that rejection alone reproduces the message verbatim. An assistant turn with ordinary
content and no `reasoning_content` offends neither upstream, z-ai accepts a tool-call
turn either way, and `thinking:{"type":"disabled"}` is honored by z-ai and by DeepSeek
— on DeepSeek it also makes the offending history succeed. A tool-using beat is
therefore the whole exposure: turns authored under `?thinking=false` carry no
`reasoning_content`, and the first one that falls through to DeepSeek fails.

A surplus `reasoning_content` is accepted by both upstreams with thinking disabled, so
only the missing direction needs answering.

Injecting `thinking` is not an option for the remaining work either: Google's
OpenAI-compatible endpoint answers `400 Unknown name "thinking": Cannot find field`,
and it is first in the `review-standard` pool.

## Work

- **Bind a conversation to its target** — the fix rather than the guard, since
  routing around only helps while something compatible remains. Tracked in
  [[bind-a-conversation-to-its-target]]; this case is why it wants the
  session-identity half without waiting for the capacity model that only
  *admission* needs.

A pool whose targets agree about thinking would stop the symptom and is not the fix:
thinking is a per-target economic call — enable where it is cheap, disable on a model
strong enough without it — so a mixed pool is what a pool is for.

Whichever lands, note what can guard it: a per-request rule is testable today, a rule
about conversations is not, because understudy cannot tell one conversation from two.
