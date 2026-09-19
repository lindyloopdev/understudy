# Forward the session header upstream

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Per-target
request-body normalization" owns what understudy shapes on the upstream request;
headers are that too, since understudy re-originates the call and is the only
HTTP client the upstream ever sees. "Session target binding" owns the session
identity the header carries.

## The failure (first seen 2026-09-18)

opencode Zen Go now rejects a chat request carrying no `x-opencode-session`:

    upstream returned status 400: Request is missing x-opencode-session and
    cannot be routed efficiently.
    https://opencode.ai/docs/go/#where-can-i-use-it

Every request to an opencode-go backend target fails this way — lindy observed
it as every beat on such a target dying `Unavailable` (`test-corpus-coupling@project`
was the first named). The policy is new (the docs page carries 2026-09-18 as its
update date); backends that do not enforce it (z-ai et al.) are unaffected, so
far. Zen's client guidance asks for a **stable session ID per conversation**,
and its proxy guidance says to **preserve the session header when forwarding**.

Neither layer below understudy can supply the header:

- opencode sends `x-opencode-session` only when the provider ID starts with
  `opencode` (`session/llm/request.ts`, v1.15.13); lindy stages the gateway as
  provider `understudy` in opencode's config, so opencode sends the generic
  `x-session-affinity` instead.
- understudy re-originates the upstream request — `newRequest`
  (providers/openai/openai.go) sets `Authorization`, `Chat` sets `Content-Type`
  — so no inbound header reaches the upstream whatever the client sent.

## Work

- **Set `x-opencode-session` on every upstream chat-completions request**,
  sourced in order: the inbound `x-opencode-session`, else the inbound
  `x-session-affinity` (what opencode sends for a custom provider), else a value
  synthesized from the conversation key `chatCompletions` already computes — a
  headerless client still gets stable per-conversation routing.
- **Plumb the value to the provider seam.** `chatCompletions` holds the inbound
  `*http.Request`; `providers.Chat(ctx, cfg, body)` does not. Ride it on
  `providers.Config` or the context; the test capture
  (`newCapturingServer`) extends to assert the header.
- **Send it unconditionally, not per-backend.** An upstream that does not know
  the header ignores it; gating on a per-backend flag would be vocabulary
  validation, which the body-override design already refuses ("never
  vocabulary" — DESIGN.md §Understudy).

## Notes

- A failover replays the same conversation to the next target; the same session
  header on each hop is correct — the session did not change, the target did.
- Side benefit, not a driver: Zen keys prompt caching on the stable session ID,
  so repeated turns through an opencode-go target get cheaper once this lands.
