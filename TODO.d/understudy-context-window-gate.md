# Refuse a request that cannot fit any target's context window, before dispatch

**Tag:** understudy / reliability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the walk that
resolves a logical model to targets; the error table's "the request is at
fault" row (a 400 relays verbatim, `invalid_request_error`).

Empirical grounding: kronk through v1.30.3 answered an oversized prompt
wrongly on both paths — non-streaming as `500`/`server_error` with decoder
vocabulary in the message (ardanlabs/kronk#861), streaming as `200` with the
error as an in-band SSE chunk no intermediary can classify (ardanlabs/kronk#862;
the observed incident — an agent retried the identical request 3/3). Both are
fixed as of v1.31.3 (verified directly against a local instance, both
streaming and non-streaming): an oversized prompt now answers as a clean,
structured `400 invalid_request_error` on either path. This gate is no longer
a defense against a silent/unclassifiable upstream failure — the failure is
already classifiable — but stays worth building as a proactive optimization:
it saves the request/response round trip a target-then-fail loop would
otherwise spend, and can name the offending target and its window directly
rather than relaying whatever message the upstream chose.

## What to build

- **Declare `context_window` per model** under a backend's models config (the
  per-`(backend, model)` box lindy already fills for pricing/limits — see
  lindy's `[gateway.backends.<name>.models."<model>"]`). Kronk's own
  `model_config.yaml` is the authoritative source today; auto-discovery
  (kronk's Tokenize API, ardanlabs/kronk#166) is a later nicety, not a
  prerequisite.
- **Gate at forward time**: estimate the buffered request body's token count
  against the *chosen* target's declared window (per-target, not the logical
  model's minimum — the walk knows which target answers). Over the window →
  the walk treats the target as unusable *for this request*: skip to the next
  candidate; answer `400 invalid_request_error` ("prompt ~N tokens exceeds
  <target>'s <M>-token context window") only when no candidate fits. A
  single-member pin then fails fast and legibly — the pin stays a pin.
- **Estimate, don't tokenize.** Exact counts are per-tokenizer (the GGUF
  embeds the model's own), and no standard OpenAI-compatible tokenize
  endpoint exists to build on. A conservative bytes heuristic (÷3 measured
  ~3.9 B/tok on gemma-4-12b prose+code; ÷3 overstates, which refuses
  borderline requests rather than passing oversized ones) is the right
  precision for a gate whose miss modes are both cheap: an overestimate
  refuses something that might have fit; an underestimate falls through to
  the upstream's exact `400` as backstop.

## Constraint: the buffer this reads is scheduled to shrink

The gate reads bytes the request path already buffers for failover replay
(`io.ReadAll` under `maxRequestBodyBytes`, understudy.go). That eager buffer
is [[understudy-streaming-body-replay]]'s planned removal: once the body
streams, `len(bodyBytes)` silently degrades to whatever the tee happened to
hold. Either hook the sizing at the tee point (the full body passes it once,
and the cap is enforced there anyway), or size only the buffered prefix and
accept the gate degrades to advisory. Decide this with — not after — the
streaming swap; building against an invariant scheduled to disappear is the
trap.

## Non-goals

- **Not prose-sniffing upstream errors.** Classifying kronk's broken
  500/in-band text by substring would import vendor vocabulary the error
  table exists to keep out, and dies the day the upstream fix lands. The
  gate compensates by *prevention* (the request never reaches the broken
  path), which survives the fix as an honest fast-refusal.
- **Not opencode compaction.** That handles accumulation across turns
  (lindy declares model limits to opencode for it);
  a birth-oversized single request has nothing to compact.
