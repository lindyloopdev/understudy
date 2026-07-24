# Per-transcript-line served-model provenance: the token-field join key

**Status:** design settled (DESIGN.md), feasibility spike-confirmed, unbuilt.
**Mechanism (affirmative):** [DESIGN.md §Served-model provenance](../DESIGN.md#served-model-provenance).
**Work items:** [TODO.d/understudy-provenance-reporting.md](../TODO.d/understudy-provenance-reporting.md).

This note is the **empirical record** behind that design — which fields survive
opencode, the spike data, and the rejected channels. The affirmative mechanism lives
in DESIGN.md; it is not restated here.

## Goal

Correlate **each transcript line to the concrete `(backend, model)` that actually
served it** — including after an understudy failover, where the served target
differs from the requested logical name. This is o11y, not billing: the operator
wants to know which model produced which turn.

## Why the obvious channels are all dead

understudy is a **separate process** behind opencode; lindy only sees what opencode
publishes on its SDK event stream. lindy learns the model from
`message.updated` → `info.ProviderID+"/"+info.ModelID` (`internal/opencode/events.go`),
which is stamped at message-creation from the **requested** model
(`understudy/<logical>`) and never reconciled with the response. Verified against the
pinned opencode binary (bundled JS) and a live spike:

- **Response headers** understudy adds → opencode drops them on the success path.
- **Response body `model`** → the openai-compatible provider ignores it
  (`modelId:this.modelId`); opencode never overwrites the message model from the
  response.
- **`usage.cost`** → for `@ai-sdk/openai-compatible` (how lindy configures
  understudy) it is **ignored**; cost is computed from the model price map, which is
  0 for understudy models. (Only the OpenRouter npm integration reads `usage.cost`,
  and only into `providerMetadata.openrouter`, which lindy's SDK `AssistantMessage`
  cannot read.)
- **Arbitrary response fields** (`system_fingerprint`, response `id`, custom keys) →
  stripped (`unknownKeys:"strip"`); `providerMetadata` passthrough needs a
  code-configured `metadataExtractor` (a function, not a JSON config option).
- **No per-request join key** crosses the boundary: no upstream request/response id
  is surfaced, no correlation header forwarded. Order-zipping (Nth request ↔ Nth
  turn) desyncs because opencode makes extra understudy calls (title/summarize) and
  can retry.

So per-line attribution is impossible via the *model identity* fields without an
opencode fork.

## The spike result: token detail fields pass through faithfully

`internal/opencode/spike_passthrough_test.go` drove real opencode
(`@ai-sdk/openai-compatible`) against a mock that stuffed a distinct sentinel into
every response field. What opencode stored on the assistant message:

| sent (`usage.*`)          | stored (`tokens.*`)   | fidelity                          |
|---------------------------|-----------------------|-----------------------------------|
| `cost`                    | `cost` = **0**        | **ignored** (price-map, = 0)       |
| `prompt_tokens_details.cached_tokens` | `cache.read` | **byte-exact, independent** ✅ |
| `completion_tokens_details.reasoning_tokens` | `reasoning` | byte-exact ✅         |
| `prompt_tokens`           | `input = prompt − cached` | derived                       |
| `completion_tokens`       | `output = completion − reasoning` | derived               |
| `id` / `model` / `system_fingerprint` | —        | dropped                           |

`tokens.cache.read` and `tokens.reasoning` survive with **full integer fidelity**
(JSON number, exact to 2^53). That is the seam.

## Why `cache.read` is the carrier (evidence for the DESIGN choice)

Empirical properties that make `cached_tokens` → `tokens.cache.read` the chosen field
(the mechanism itself is in DESIGN.md §Served-model provenance):

- **Faithful & independent** — the spike stored it byte-exact, unaffected by the other
  counts.
- **Free to overload** — lindy today computes `StepEnd.Tokens = input + output +
  reasoning` (`events.go`) and drops `cache.read` entirely, so repurposing it corrupts
  nothing currently read. (`reasoning` is a faithful backup carrier, but lindy folds it
  into the visible token sum, so it isn't free.)
- **Ample uniqueness** — a JSON number is exact to 2^53, ≫ the ~2^16 concurrent-request
  target; collisions only matter within a request's correlation window.
- **Derived input is ours to zero** — opencode derives `input = prompt_tokens −
  cached_tokens`, so setting `prompt_tokens` to the id alongside `cached_tokens = id`
  makes `input` read a clean **0** rather than a bogus figure; the real prompt/cached
  counts are destroyed on-band and ride the provenance record instead. (An earlier
  design *compensated* — `prompt_tokens = real_non_cached_input + id` — to keep `input`
  honest on-band. Dropped once real usage moved to the side-channel: honest on-band input
  bought little given the served model is already hidden and `cache.read` already carries
  a garbage id, and compensation coupled the id's bit-width to a float64 headroom the sum
  had to stay under. Zeroing removes both.)

## Open questions / risks

- **Response-tail rewrite** is more involved than the request-side `rewriteModel`
  prefix-scan: `usage` is last, and the streaming (SSE) vs non-streaming shapes differ.
  Confirm opencode always sends `stream_options.include_usage` (spike showed usage
  present).
- **Correlation-stream scoping** rides the per-token/tenant direction of
  [[understudy-shared-daemon-subserver]]; needs per-token isolation so a tenant sees
  only its own records.
- **Charter expansion (contained).** The `usage` synthesis is **lindy policy**, kept
  out of understudy: understudy gains only a generic response-interceptor seam
  ([[understudy-response-interceptor]]), and standalone proxy users (nil interceptor)
  are unaffected. The rewrite itself — response tail-splice, id generation, the stream
  — lives in lindy's interceptor.
- **Provider-config coupling.** The carrier fidelity is a property of
  `@ai-sdk/openai-compatible`'s usage normalization (`input = prompt − cached`). A
  future provider-config change (or switching npm providers) must re-verify the spike.
- The JSONL stream's real-usage payload also feeds [[understudy-cost-metering]] and
  the token-usage observability note in [[understudy-scope]] — one side channel, three
  consumers.

## Rejected alternatives (why not)

- **Overload `usage.cost`** — dead on the compat provider (computed, = 0); OpenRouter
  route lands only in unreadable `providerMetadata`.
- **Patch the opencode fork** to stamp the served model onto each message — works, but
  cross-repo fork maintenance for something the token-field trick achieves host-side.
- **Beat-level correlation** (per-container token as grouping key) — coarser than the
  per-line goal; the token field gives per-step precision the grouping key can't.
- **Order-zipping** request sequence to turn sequence — desyncs on retries and
  opencode's side calls.
