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

**Delete the inference and the reserved name.** In `chatCompletions`, a slash-less
name is either a declared logical model or a 404. `firstCandidateBackend`, the
catalog fetch it feeds, and the empty-catalog 502
(`backend %q advertises no models`) go with it, as does the `DefaultLogicalModel`
constant. This also retires the live `/v1/models` call the chat path makes merely
to resolve a name.

Test surface: the cases driving `default` through the inference —
`TestChatCompletionsForwardedModel`'s sole-backend and alphabetically-first-backend
cases, and `TestChatCompletionsValidation`'s registered-provider-catalog case —
assert a resolution that no longer exists. Each needs a declared logical model, or
deletion where the inference *was* the subject.
`TestChatCompletionsLogicalModelResolutionError` covers catalog-fetch failures
reachable only through the inference.

## Open

**What replaces `Config.DefaultModel`.** It returns `DefaultLogicalModel` whenever
a backend is configured, and lindy calls it — `cmd/lindyd/run.go` and
`cmd/lindy/main.go` pass the result to the Docker runner as the model opencode
should request — so it cannot simply vanish. Two shapes:

- **Delete it.** lindy names the logical model it wants from its own configuration.
  Purest: understudy holds no opinion about which of its models anyone requests.
  Costs lindy a config field, and an operator loses the ability to designate one
  beside the model definitions they just wrote.
- **Repoint it at an operator-declared field** — `[understudy] default_model =
  "fast"` — validated only to name a *declared* logical model. That is a
  consistency rule about the document rather than reserved vocabulary, and lindy's
  call sites keep working with a semantic change.

Not an option: requiring a stanza named `default`. Mandating the name while
claiming understudy gives it no significance is the contradiction this item exists
to remove.

## Out of scope

- **The shipped example configuration** that makes a first run work without
  hand-writing TOML — the replacement for the inference, already tracked in
  [[free-tier-drop-in-config]].
- **Skipping an unusable backend**, which loses its default-inference vehicle here:
  a logical model whose targets span two backends is what exercises it. See
  [[degrade-past-a-misconfigured-backend]].
