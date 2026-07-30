# Remove the reserved default model and the catalog inference behind it

**Tag:** understudy / routing / config

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "understudy never
picks a model, and reserves no model name", the paragraph after it on the
configless guarantee being structural, "Availability, not quality" (the boundary
reserving model choice for the orchestrator), and "Model addressing" (slash-less
names as operator-declared selectors).

`DefaultLogicalModel` is a name understudy treats specially, and a slash-less
request for it resolves by inference when no stanza declares it: `chatCompletions`
picks the lexically first routable backend and forwards that backend's **first
advertised** model. understudy cannot judge what it is picking — `/v1/models` is a
flat list of ids with no capability metadata, so the pick can be an embedding,
audio, or image model, and the caller gets a provider error about an unsupported
endpoint with nothing tying it back to a model they never chose.

## Build path

Two commits, in this order — the constant has two users and the inference gate is
one of them, so the second cannot precede the first.

**Delete the inference.** In `chatCompletions`, a slash-less name is either a
declared logical model or a 404. `firstCandidateBackend`, the catalog fetch it
feeds, and the empty-catalog 502 (`backend %q advertises no models`) go with it,
retiring the live `/v1/models` call the chat path makes merely to resolve a name.

Test surface: the cases driving `default` through the inference —
`TestChatCompletionsForwardedModel`'s sole-backend and alphabetically-first-backend
cases, and `TestChatCompletionsValidation`'s registered-provider-catalog case —
assert a resolution that no longer exists. Each needs a declared logical model, or
deletion where the inference *was* the subject.
`TestChatCompletionsLogicalModelResolutionError` covers catalog-fetch failures
reachable only through the inference.

**Delete `Config.DefaultModel` and `DefaultLogicalModel`**, with
`TestConfigDefaultModel`. Which of its logical models a consumer requests is not
understudy's question to answer, so the method answering it goes rather than being
repointed at a config field understudy would store and never act on. Requiring a
stanza named `default` is likewise not the answer: mandating the name while
claiming understudy gives it no significance is the contradiction this item exists
to remove.

lindy pins understudy by pseudo-version with no `replace`, so removing the method
breaks nothing until lindy bumps, and the compile error then lands on the two call
sites that need changing.

## What lindy owes on its side

Tracked in that repo, not here, but recorded because the constraint is easy to
miss: `DefaultModel`'s **empty return is load-bearing**. `stageOpencode` uses it as
a mode discriminator — non-empty declares the understudy provider in the staged
opencode config *and* selects the session default; empty stages a minimal config
and leaves opencode on its own model, which is the configless bypass. So lindy
needs a field of its own naming the logical model agents request, empty when
`len(cfg.Understudy.Backends) == 0` (an exported field, so directly checkable),
and a load-time rejection when backends are configured and the field is not — else
lindy silently bypasses an understudy the operator configured, trading away exactly
the guarantees the configless note warns about.

## Out of scope

- **The shipped example configuration** that makes a first run work without
  hand-writing TOML — the replacement for the inference, already tracked in
  [[free-tier-drop-in-config]].
- **Skipping an unusable backend**, which loses its default-inference vehicle here:
  a logical model whose targets span two backends is what exercises it. See
  [[degrade-past-a-misconfigured-backend]].
