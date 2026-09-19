# Send a session header upstream

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Per-target
request-body normalization" owns what understudy shapes on the upstream request;
headers are that too, since understudy re-originates the call and is the only
HTTP client the upstream ever sees. "Session target binding" owns the
token-mixing construction reused below (mixed with the bearer token so one
tenant's value cannot collide with another's in the shared daemon) — here for a
different purpose, a header Zen consumes, not target-pinning.

## The failure (first seen 2026-09-18)

opencode Zen Go now rejects a chat request carrying no `x-opencode-session`:

    upstream returned status 400: Request is missing x-opencode-session and
    cannot be routed efficiently.
    https://opencode.ai/docs/go/#where-can-i-use-it

Every request to an opencode-go backend target fails this way — lindy observed
it as every beat on such a target dying `Unavailable` (`test-corpus-coupling@project`
was the first named). The policy is new (the docs page carries 2026-09-18 as its
update date); backends that do not enforce it (z-ai et al.) are unaffected, so
far.

## Why not forward the client's header

Investigated and rejected: relay whichever of `x-opencode-session`/
`x-session-affinity` the inbound request carries, falling back to a synthesized
value only when neither is present.

- Neither layer below understudy can supply it anyway: opencode sends
  `x-opencode-session` only when the provider ID starts with `opencode`
  (`session/llm/request.ts`, v1.15.13); lindy stages the gateway as provider
  `understudy`, so opencode sends the generic `x-session-affinity` instead. And
  understudy re-originates the upstream request — `newRequest`
  (providers/openai/openai.go) sets `Authorization`, `Chat` sets `Content-Type`
  — so no inbound header reaches the upstream whatever the client sent,
  regardless.
- Even where available, forwarding the client's value is worse than generating
  our own: opencode's `x-session-affinity` is a session-lifetime id that
  survives compaction unchanged, while understudy's own conversation-key
  affinity deliberately resets at compaction (DESIGN.md, *Affinity is a
  short-lived hint, not a lease*) — exactly when the pinned target's cache goes
  cold. Forwarding the client's value would tell Zen one continuous session
  spans a boundary understudy itself treats as broken.
- Zen's own "preserve the session header when forwarding" guidance
  (opencode.ai/docs/go/, Validated Clients) is Codex-specific — "Some versions
  and proxy setups still omit it; preserve the session header when forwarding
  requests" — about not dropping a header a proxy already has, not a mandate
  that any proxy relay the client's literal value. A design that never omits
  the header doesn't trigger this concern.
- Backends are operator-configured, not per-tenant (`Backend.Config.APIKey`) —
  every lindy tenant routed to the same named backend authenticates to Zen as
  the same account. Forwarding a client-chosen value gives understudy no
  control over two different tenants' conversations colliding on Zen's side;
  generating our own lets understudy guarantee they don't.

## Work

- ~~Plumb a `sessionID` parameter to `Handler.Chat`/`openai.Chat` and gate
  the header on the base URL's host being opencode.ai~~ — done.
- **Always compute the value in `chatCompletions`** (understudy.go:2189,
  currently passes `""`), unconditionally — no inbound-header inspection. Mix
  the bearer token into the conversation key the same way the internal
  affinity key already does (DESIGN.md, "mixed with the bearer token so one
  tenant's affinity cannot steer another's routing in the shared daemon"), so
  two tenants' conversations can never collide on the value sent to Zen. This
  also fixes the compaction issue above for free: the value changes exactly
  when `conversationKey` does.

## Notes

- A failover replays the same conversation to the next target; the same session
  header on each hop is correct — the session did not change, the target did.
- Side benefit, not a driver: Zen keys prompt caching on the stable session ID,
  so repeated turns through an opencode-go target get cheaper once this lands.
