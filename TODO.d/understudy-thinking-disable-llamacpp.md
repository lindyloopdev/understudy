# Per-target thinking-disable: also emit enable_thinking for llama.cpp backends

**Tag:** understudy / fallback

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Per-target
request-body normalization." Sibling item:
[understudy-thinking-disable.md](understudy-thinking-disable.md) (the tri-state
enable side). Consumer context:
[notes/2026-08-08-kronk-gemma4-local-stack.md](../../../lindy/notes/2026-08-08-kronk-gemma4-local-stack.md)
in the lindy repo.

## The gap (verified 2026-08-08, lindy↔kronk)

`?thinking=false` injects the **Anthropic shape** `"thinking":{"type":"disabled"}`
(`disableThinking`). llama.cpp-style servers — kronk, and likely llama.cpp server
/ vLLM — use a different param, `enable_thinking` (boolean), and **ignore** the
Anthropic shape. Verified against kronk (gemma-4-12b, default thinking-on):

- request with `"thinking":{"type":"disabled"}` → **27 reasoning tokens**
  (ignored — same as the 28-token control).
- request with `"enable_thinking":false` → **0 reasoning tokens** (honored).

So `?thinking=false` is a **silent no-op against llama.cpp backends** — a target
like `kronk/gemma-4-12b-it-Q6_K?thinking=false` disables nothing, and (for Gemma
4) the model then generates 4000–5800 reasoning tokens, hits `max_tokens`, and
emits no usable output. lindy currently works around this with a kronk-global
`enable_thinking: false`, which is the wrong layer (server global, not
per-target) and blocks any selective thinking-ON.

## Fix

Emit the llama.cpp param alongside the Anthropic one — no new concept, just both
shapes so each backend picks up the one it understands:

- `?thinking=false` → inject **both** `"thinking":{"type":"disabled"}` **and**
  `"enable_thinking":false`.
- When `?thinking=true` lands (see
  [understudy-thinking-disable.md](understudy-thinking-disable.md)), emit
  `"enable_thinking":true` on the enable side too.

Alternative: a distinct pass-through target param `?enable_thinking=true|false`,
kept separate from the Anthropic-connoted `thinking`. Cleaner if `thinking`
should stay Anthropic-only, and it's the more general surface (true/false, vs
`thinking` being disable-only/reserved today).

Either way, the request-body normalizer (`disableThinking` and its future enable
counterpart) is where both shapes get injected. No backend sniffing needed —
injecting both is harmless to backends that key off only one.
