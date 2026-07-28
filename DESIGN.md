# Understudy

## LLM API Keys via Understudy <a id="llm-api-keys-via-understudy"></a>

LLM API keys never enter the container. The agent (OC/opencode) points its
base URL at **understudy** — the host-side reverse proxy
(`internal/understudy`). The in-container agent presents only a scoped,
per-scene token (`LINDY_SESSION_KEY`); understudy validates it and injects the
real upstream credential on the outbound call to the LLM provider, so the raw
key never crosses into the container.

This follows from the container-as-security-boundary principle: the agent
process executes arbitrary shell commands, so any credential accessible to the
process (env var, file, Docker secret) is accessible to agent-generated code.
The only way to keep a credential out of reach is to keep it out of the
container entirely. Understudy is **mandatory in-path** for sandboxed execution:
the sandbox removes opencode's no-provider escape hatch (its ambient auth is
nothing inside a container), so without the host-side broker the agent would
have no auth at all. Net threat model: free `lindy` is strictly better than
running the agent directly on the host — credential exposure is identical-or-
lower, with container + worktree isolation added on top.

Understudy also enables per-run budget caps, audit logging of all LLM calls,
and an immediate kill switch (stop understudy, agent loses LLM access).

The token understudy validates is a **per-scene credential**: the caller mints
`LINDY_SESSION_KEY` (`serveRun` for free `lindy`, the gRPC `StartScene` handler
for `lindyd`) and hands it to the single-Scene executor, which injects it into
the container and *is* the validator the host-side understudy consults — the
injected token and the validated token are one and the same, live only for the
scene. The composition root serves the understudy handler (standalone for free
`lindy`, multiplexed onto the control-plane listener for `lindyd`) but always
points it at the executor's mint, so free and paid share one credential-broker
path and differ only in transport. A daemon may additionally wrap the validator
with a static operator token for external (non-engine) clients; the engine's
mint is the fallthrough either way.

**Config handling is two stages: load, then validate.** *Loading* gets the
configuration into the struct — decode the TOML, parse the base URL and each
target into their types, and fill each backend's key from whichever source it
names. Reading a file or an environment variable to populate a field is part of
loading, in the same sense that decoding the document is; it is one more level of
indirection, not a separate phase. Loading fails only where loading is
*impossible*: a malformed document, an unreadable path. *Validation* then runs
over the loaded struct — tags for per-field shape, `Validate() error` methods for
the relationships tags express poorly — and carries every rule about whether the
configuration is acceptable.

**Loading never discards the source fields.** A backend that names
`api_key_env = "GROQ_API_KEY"` still carries that name after its key is filled,
so validation sees both the empty key *and* where it should have come from, and
says which variable to set. Keeping one struct through validation is what makes
the diagnostic specific; transforming into a credential-only type would discard
exactly the fact the operator needs.

**Credential sourcing.** Each backend's upstream key lives in the resolved
understudy config. A lindy agent can read both the config file and the host
environment, so to keep the plaintext out of the agent's reach a backend may
reference the key by file instead of inlining it: it sets exactly one of
`api_key`, `api_key_file`, or `api_key_env`. `api_key_file` resolves to an absolute path — a
leading `~/` expands against the home directory, an otherwise-relative path
against the config file's directory — whose contents supply the key. Placing
that file outside the worktree and unmounted from the container keeps the secret
where agent-generated code cannot read it. `api_key_env` names an environment
variable holding the key; it names the variable rather than interpolating its
value so that *declaring* a credential stays distinguishable from *resolving*
one — the distinction `auth` below depends on. An empty or whitespace-only file, an
unreadable path, a non-absolute path at understudy's resolve, or setting more than one
source is a config error.

**Credential requirement (`auth`).** A backend declares whether it needs a
credential at all. The value decides **what kind of fact an absent credential
is** — a defect in the document, or a fact about the world:

- `required` (the default) — an empty key is a **config error**. Preserves the
  strict behavior for a hand-written config, where a credential that fails to load
  is a typo, not an expectation.
- `auto` — an empty key is a **valid configuration**; the backend is simply
  **unavailable**. This is what makes a shared config
  (`examples/free-tiers.toml`) drop-in: an operator holding one provider's key
  gets that provider and a startup that succeeds.
- `none` — no credential is wanted (a local ollama or LM Studio), so there is
  nothing to be absent. Naming a key source is a config error, since it could
  never be read. If the upstream turns out to demand a credential after all, its
  `401` is the diagnostic — understudy adds no special handling.
- `optional` — **reserved, rejected.** Send a credential when one loads, stay
  available when none does. Its name is held so that `auto` does not drift into
  meaning it.

**`auth` is not a config transformation.** A backend whose variable is unset is
still in the configuration — the document said it exists, and that remains true.
Nothing is pruned: no backend is removed, no target naming it is rewritten, no
logical model is emptied. What changes is *availability*, which understudy already
models for a demoted or rate-limited target — a backend with no credential is
unusable in the same way, and the failover walk passes over it for the same reason.
An operator who exports the variable makes it usable again without the
configuration having changed at all.

That also keeps `auto` distinct from an upstream *rejecting* a credential. `auth`
governs only whether an absent credential is an error; a credential that loads and
is then refused is a defect the operator can act on, logged at ERROR when the
target is demoted, regardless of the backend's `auth` value.

**Transport encryption.** understudy serves the agent over **TLS, not cleartext**.
The container reaches it across the Docker bridge — a segment other, untrusted
agent containers share — so an unencrypted hop would leak the per-scene token (a
bearer credential) and the entire request/response (prompts, repository code) to
any co-resident container sniffing the bridge, defeating the isolation the
container wall otherwise provides. understudy presents a **self-signed
certificate** — ephemeral per run, or a per-`$HOME` CA for the shared daemon
(§Understudy) — and the container **trusts that CA explicitly**: the CA is
bind-mounted read-only and named to opencode via `NODE_EXTRA_CA_CERTS`.
Verification is never disabled (`NODE_TLS_REJECT_UNAUTHORIZED=0` would reopen the
same segment to a MITM). The certificate's SAN covers the stable hostname the
container dials (the `ExtraHosts` reachability entry, §Container Lifecycle), not
the dynamic bridge IP. The opencode/Bun trust path is validated in
[notes/2026-07-10-opencode-bun-self-signed-tls-spike.md](notes/2026-07-10-opencode-bun-self-signed-tls-spike.md).

This is the path when understudy is configured. In configless mode (no
`[understudy]` backends), opencode connects to its own keyless free trial
directly — understudy is bypassed, so the budget caps, audit logging, and kill
switch above do not apply, and prompts and repository code go to a third-party
provider. Configless trades these guarantees away for a zero-config tryout; see
`examples/lindyd.toml`.

## Understudy <a id="understudy"></a>

Beyond the credential broker above, understudy is the model proxy in front of the
agent — a standalone OpenAI-compatible reverse proxy, embeddable in
`lindy`/the Director/standalone, with pluggable token auth.

**Availability, not quality.** understudy owns *availability* — resolving a model
name to a reachable target, hiding the credential, rejecting a stalled upstream,
and (later) failing over. It never owns *quality*: *which* target is good enough
for a task is the orchestrator's judgment. understudy resolves and executes the
target(s) it is handed; it never picks one on merit. That boundary is why model
*selection* — cost/quality tradeoffs, calibration — lives in the orchestrator,
not here.

**Model addressing.** The agent requests `understudy/<model>`:

- **Slashed** (`<backend>/<model>`) — a **direct target**: a concrete model on a
  named backend. The escape hatch, and how per-beat pins are addressed today.
- **Slash-less** (`<model>`) — a **logical model** name understudy resolves to a
  target. A logical model is an operator/orchestrator-composed, config-defined
  selector; the name is the whole interface.

**Availability failover.** A logical model resolves to a **priority-ordered
candidate list** (`BackendConfig.Models`) that understudy **fails over** across.
understudy routes each request to the first target not currently
failing past a threshold, tracking a failing-since per canonical `(url + key + model)` and
classifying a 502/connection error as an availability failure. understudy **walks** the list,
advancing to the next target when an attempt is **classified an availability
failure** — a hard error (502/connection) or a rate-limited target. **Stalls: two axes, three dispositions.** Whether a stalled request can be
salvaged turns on two independent facts. **Replayability** is set by the header
boundary: *pre-header* (no first byte yet) means nothing is written to the client,
so the request is replayable to another target; *mid-stream* (headers, maybe
partial payload, already sent) means it is not — `WriteHeader(200)` cannot be
un-sent. **Switching cost**, when replay *is* possible, is set by conversation
position: a *first request* has no prompt-cache or coherence to forfeit (~0), a
*subsequent request* forfeits the pinned target's warm cache and mid-conversation
coherence (high). Mid-stream collapses the cost axis — replay being impossible,
position is moot — leaving three cases:

1. **First-request pre-header stall** — replayable, cost ~0 → **synthesized
   backpressure with an eager replay**: synthesize a bounded `Retry-After`,
   demote the target to `readmitAt`, replay the triggering request to the next
   candidate, recover via the half-open probe. *(The busy-local-model case.)*
2. **Subsequent-request pre-header stall** — replayable, but switching forfeits
   coherence → the same disposition **gated by a coherence-sized wait budget**:
   hold in place longer, replaying only once the budget is spent. This is
   §Affinity's "wait budget driven by coherence" (a live session's budget is
   large, a one-shot's ~0) applied to a stall rather than a 429.
3. **Mid-stream stall** — not replayable → **cancel-and-surface, no demotion**:
   the idle deadline cancels the hung request so it never hangs forever, but the
   target is not demoted. A ~5m-expensive, recovery-undetectable, mid-response
   signal is too weak a basis for a binary verdict, so persistent mid-stream
   slowness surfaces as *cost* in the selection layer (a slow target sinks in the
   cost order and re-floats as it recovers), never as a demotion here.

Each
entry is a full `(backend, model)` target, so a
cross-provider hop carries its own model swap. The list is **author-ordered**,
which is also how cost preference is expressed: cheapest first, pricier
equivalents reached only when the cheaper are down. Resolution (ordering the
candidates) stays distinct from execution (walking them), so later ordering
policies — cost, round-robin, per-session affinity, per-backend health — enrich
resolution without disturbing the walk. On a fatal failure understudy **retries
the current target first** (a single 5xx is a blip) under a synthesized backoff
(below), and **fails over only once that target has been failing past a
threshold** — a *duration* (≈15s), not a failure count, tracked per
canonical `(url + key + model)`. That order is itself a cost-derived threshold: an equal-cost
alternate → ~0 (fail over at once), a pricier one → retry the cheaper target
longer. A later, larger threshold (≈2m) hard-fails to the terminal reject when no
alternate remains.

**Session target binding.** Within a session understudy pins one target and
reuses it — no per-request re-selection. It re-walks the list (from the top,
first available) **only when the pinned target fails to serve** — having been
failing past the failover threshold — orphaning the session and re-pinning.
Neither a slow nor a stalled target is swapped out by the availability layer:
the idle deadline cancels a hung request but does not demote its target (a stall
is a cost signal, not an availability verdict), and switching mid-session
forfeits prompt-cache and coherence, an unquantifiable cost that lindy's small
isolated beats bound anyway. Because a forced re-pin already
pays that cost, it reconsiders the whole list (a recovered-cheaper target is
eligible again), which is why the re-walk starts from the top. The candidate list
is **orchestrator-composed and config-resident**: opencode interposes between the
orchestrator and understudy and forwards only the model name, so the name is
understudy's sole per-request input and all routing policy (membership, order,
per-hop thresholds) lives in understudy's config — written by the
orchestrator/calibration, not passed per request. understudy stays beat- and
price-blind; it executes availability, never selection.

**Per-target request-body normalization.** A logical model's candidate list is
heterogeneous — a non-thinking primary and a thinking fallback answer to
different request-body contracts — yet the agent builds one body, shaped for the
model it *named*, blind to which target understudy actually pins. understudy is
the only layer that knows the pinned target, and the shared choke point in front
of every client, so per-target body adaptation lives here, not in opencode. It is
applied **only to the pinned target**, never blanket across the logical model:
the primary's body forwards untouched, so an unrecognized field is never sent to
a backend that might reject it.

Overrides ride **inline on the target reference** as URL-style query params —
`<backend>/<model>?thinking=false`. Decoding the reference is **purely
structural**: a `Target` carries its backend, model, and raw query, nothing more —
no feature policy leaks into the transport parse. The target's `(backend, model)`
is what gets **dialed**; **availability** keys on the canonical upstream account +
model (`healthKey`) — both projections that drop the query overrides, so the
thinking and non-thinking profiles of one model share health (same endpoint) while
carrying different body policy.
That projection is what lets the *same* model appear as distinct profiles across
logical models — a home a global per-model flag (bakes one behavior in) or a
logical-model-per-profile (nests logical models) can't provide. The **domain
rules** — the `thinking` value's semantics — are validated where the config
**resolves** (`Config.Resolve`), beside the existing backend-reference check,
never in the decode. An **unrecognized query param is ignored, not rejected**
(Postel's law): rejecting it would break forward/backward compatibility — an
older binary would refuse a config carrying a param it hasn't learned yet, so a
new override couldn't roll out across a fleet ahead of the binary. Only *known*
params are validated (e.g. `thinking` must be a strict boolean).

The first override is **`thinking=false`** → understudy injects
`thinking:{type:disabled}` into the forwarded body. Thinking fallback targets
otherwise burn reasoning tokens and, for some, emit chain-of-thought into
`content` where it collides with the fenced findings block reviewers parse;
disabling restores the primary's non-thinking behavior on the failover hop.

The override value is a **strict boolean**: a value that doesn't parse is
rejected when the config resolves, so a config typo fails loudly rather than
silently no-opping. Only `false` is honored — `thinking=true` (forcing thinking
*on*) is **reserved and rejected at resolve**, not a silent no-op: honoring it
means injecting
`thinking:{type:enabled}`, which is load-bearing only for a reasoning model that
defaults thinking *off* (hybrid/opt-in reasoning models exist), a behavior not
yet built. Rejecting `true` today keeps that evolution forward-compatible —
error→valid never changes an existing config's behavior, where no-op→inject
would. The override is therefore modeled **disable-only** until enable lands with
its own behavior. This is the first member of a growing category — later
normalizations (dropping a `temperature` a strict target rejects, downgrading
`response_format`, renaming `max_tokens`) likewise land with their own behavior.

**The `default` logical model.** `default` is the reserved logical model lindy's
built-in beats request. Resolution is mode-dependent:

- **Configless** (no `[gateway]` backends) — understudy is bypassed; opencode uses
  its keyless free trial (per the credential-broker note above). No logical models.
- **Configured** (≥1 backend) — `default` resolves to a **configured target**: the
  operator's explicit `[gateway.models.default]` target list if set, otherwise a
  fallback to a model from a configured backend's advertised catalog.
  It **never** silently falls back to the configless free trial — the guarantees a
  configured operator is paying for (budget caps, audit, kill switch, egress
  control) must not be silently traded away. If no configured target resolves at
  all, that is an error, not a free-trial fall.

**Rate-limit reject.** A long upstream `Retry-After` (429/5xx) is converted to a
non-retryable **400** before the agent sees it — opencode honors `Retry-After`
with no ceiling (~24.8 days) and lindy can't detect the situation from the event
stream, so the reject must live in the proxy. The response splits status (400 =
retry-control for the agent) from envelope `type` (the reason for lindy, e.g.
`upstream_rate_limited`).

**Synthesized backoff (no `Retry-After`).** The reject's converse: a *retryable*
failure carrying **no** `Retry-After` — a 429 without the header, or a 5xx — is
not relayed raw either. Unhandled, opencode hammers rapid retries at a failing
upstream (or, on its unbounded path, hangs). understudy instead **synthesizes** a
`Retry-After` and injects it while preserving the retryable status, so opencode
backs off *understudy's* interval instead of its flat 30s. The interval grows
exponentially per backend, is jittered, and resets on success; its ceiling **is
the rate-limit-reject threshold**, so on crossing it the response becomes that
same terminal 400 — one threshold caps both paths. This makes understudy an
active backoff *controller*, not a pure header relay — still an availability
control, not a quality one.

**Standalone-proxy idle shutdown.** The standalone `lindy proxy` daemon
(§Binaries) self-terminates after an idle window with no requests — crash-safety
so a proxy orphaned by a dead parent doesn't run forever. Each served request
resets the countdown, so the window bounds *inactivity*, never a busy proxy. The
window is **policy, not a fixed constant**: a single-user CLI defaults to
minutes; a hosted deployment disables it (the daemon is meant to persist); and a
single-user setup that wants to keep understudy's learned per-account
rate-limit/concurrency state (§Concurrency & Rate Limiting) warm across long gaps
sets it to hours, so that state isn't discarded and re-probed. **Disabled installs
no guard** — the proxy runs until its context is cancelled; a positive window
installs the guard.

**Rate-limit demotion is quota-class-aware.** A 429's advertised delay is not
always honest about when the target recovers, so demotion keys on the *quota
class*, read from the structured `QuotaFailure.quotaId` in the body (Gemini's
OpenAI-compat 429 carries the native `google.rpc` details — see
[notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md](notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md)).
A **per-day** exhaustion (`…PerDay…`, e.g. free-tier RPD) advertises a misleading
sub-minute `retryDelay` yet does not reset until a fixed daily boundary (midnight
Pacific for Gemini), so understudy benches the target until *that boundary*, not
the advertised delay — instead of re-probing every ~40s into a wall it can't
clear for hours. A **per-minute** throttle (`…PerMinute…`, RPM/TPM) keeps its
short advertised delay: that window really does clear in seconds. The advertised
delay stays the demotion input everywhere else; the quota-class override applies
only where that delay provably lies. (The delay itself is still read from message
*prose* — the structured `retryDelay` is present but has flipped in and out across
endpoint changes, so prose remains the resilient source; the quota id is the one
structured field we depend on.)

**Request disposition: a Retry-After ladder.** Given a retryable failure and a
healthy next target, understudy's disposition is gated by the remaining
`Retry-After` against a **wait budget** — the tolerable in-request delay before
switching, bounded by the client's own timeout and widened by the cost of the
cheapest fallback (a dear fallback is worth waiting longer for):

- **≤ wait budget → wait in place.** Sleep out the throttle and retry the *same*
  target; the client sees a slow success, never the 429, and the preferred model
  (its prompt cache, its coherence) is preserved. *(Staged — the transient-absorb
  refinement. `T_wait` is decided empirically against held-connection cost, not
  guessed; today a short throttle is instead relayed with the synthesized backoff
  above.)*
- **> wait budget, next target healthy → fail over in place.** Serve *this*
  request from the next target rather than returning the error — this is what
  makes a sustained-rate-limited or per-day-benched target invisible to the
  client. **Within-request failover is orthogonal to demotion:** it decides the
  current request's disposition; whether the target is *also* benched for future
  requests is the separate quota-class / threshold decision. A 5xx *blip* is still
  retried in place under the failover threshold — only an immediate/sustained
  demotion fails the current request over. Replaying to the next target requires
  retaining the request body: buffered eagerly today, with a streaming sole-reader
  form planned (the body must never be shared with the outbound transport, or the
  drain races its writeLoop — [[understudy-streaming-body-replay]]).
- **list exhausted → surface / reject.** No target left: relay the retryable
  failure with a synthesized backoff, or, past the terminal threshold, the
  non-retryable 400 reject.

The ladder subsumes the reject and backoff paths as its bottom rung; failover and
wait-in-place are the rungs that keep the client on *a* model — the cheapest
healthy one, or (with affinity) its own.

**Affinity and admission (staged).** Holding a throttled request open
(wait-in-place) turns understudy from a stateless relay into an **admission
scheduler** for a scarce backend: it owns the pending requests, so it can order
them and — more valuably — *prioritize* them. When a backend has limited capacity
(a recovering RPM window, a few free slots), the requests that benefit from it —
live, coherent sessions — should be admitted and the rest shed to the fallback
sooner. That is a **per-request wait budget driven by coherence**: a live
session's budget is large (hold for its model), a one-shot's is ~0 (fall back at
once). This is the concrete realization of session binding above; two dependencies
gate it, both staged:

- **Session identity.** understudy must recognize which requests belong to one
  conversation. opencode holds a session id internally but passes **none** on the
  OpenAI-compat call — understudy sees only the bearer token and the body. So the
  key is **inferred from the payload**: a hash of the invariant leading messages
  (system + first user turn), which is also exactly what prompt-cache coherence
  keys on. **Feasibility-spike first** — confirm the leading-prefix hash stays
  stable across a real session's turns (and how often context compaction breaks
  it) before building on it.
- **A capacity model.** To admit some and shed the rest, understudy must know how
  much of the scarce backend is free — the RPM budget from the `QuotaFailure`
  signal, plus the concurrency-limiter slots. This is a **priority-aware
  allocator** over a recovering resource. It **supersedes, not revives**, the
  shelved proactive pacer
  ([notes/2026-07-11-pacer-superseded-by-retry-after-recovery.md](notes/2026-07-11-pacer-superseded-by-retry-after-recovery.md)):
  that was rejected for *conserving* a daily quota with nothing to conserve; this
  *allocates* a per-minute one to the requests that value it.

FIFO ordering alone is low-value for lindy's mostly-independent requests; the
priority allocation is the win.

**Served-model provenance.** <a id="served-model-provenance"></a> When failover
routes a request to a target other than the one named, the transcript must still
attribute each line to the model that **actually** served it. opencode relays only the
*requested* model name back to lindy — the served identity never crosses that boundary
— but a handful of response `usage` fields pass through with full fidelity. So each
served request is tagged with a unique numeric **id**, carried in the one
otherwise-unused faithful field: `cached_tokens` → the message's `cache.read` token
count. `prompt_tokens` is set to the same id, so opencode's derived input
(`prompt_tokens − cached_tokens`) reads **zero** rather than a bogus figure — the
real token usage is destroyed in the relayed body on purpose and survives only in the
provenance record below. The real `(backend, model)` and full usage ride a separate **provenance stream** —
per-token-scoped JSONL, one record per served request — which lindy joins to each
transcript step on the id.

The correspondence is **step → record**, not request → step. Every step is one
served request, so it has exactly one record; but the reverse does not hold —
opencode also issues requests that never become steps (session title/summarize,
retries), each producing its own record. Those orphan records land on the stream
too, keyed by ids no step carries, and are simply never looked up; each step is
matched to *its* record **by id**, so the join is exact at line granularity and
correct even across a failover. Positional correlation (Nth request ↔ Nth step)
cannot work precisely because of those extra requests; the id is what makes the
join robust to them.

**The join key is present exactly when the rewrite succeeded.** The interceptor
overwrites `cached_tokens` only when it can read the usage; when the usage is
unparseable it relays unchanged and writes no record, and opencode then reports a
**zero** cache-read. Since an id is a nonce ≥ 1, a non-zero carried value is
therefore always a real id with a record on the stream, and a step with no id
(zero) is one the rewrite could not tag — passed through **unattributed** rather
than mis-joined; an id-less step is never joined.

An id-bearing step is joined **by id, not by position**: a **single per-token
subscriber** drains the stream into an `id → record` map, and each step reads its
record out of that map. The map — not a positional scan — is essential because a
token's requests run **concurrently**: many transcript steps are in flight at once
(a review's parallel reviewers all share one token), so records interleave and a
step routinely reaches the consumer before its own record does. The lookup waits,
bounded, for a not-yet-arrived record; on a genuine miss — a record dropped under
buffer pressure, or never broadcast — it passes the step through **unattributed**
rather than stall. Provenance is best-effort: it never blocks or fails a transcript
step. The single per-token subscriber is also what the producer is sized for (one
reader per token, not one per in-flight request).

The stream is a **live broadcast**, per-token-scoped: each record fans out to every
current subscriber, and a subscriber sees only records produced after it subscribes.
Producing a record never blocks and needs no subscriber — a generously-sized
per-subscriber buffer absorbs bursts, and a subscriber that falls hopelessly behind
drops its oldest records rather than stalling producers; zero, one, or many readers are
all valid. That uniformity is the point: the record's several consumers (the join
above, cost metering, usage observability) each subscribe independently, and a second
reader is simply another subscriber, never a conflict to reject. There is no replay, so
a consumer subscribes before its token's requests begin.

Collision safety is a property of **scope**, not the id's size. The join matches an id
only against the **same token's** records, so an id two tenants happen to share can
never cross between them — cross-tenant misattribution (a disclosure) is impossible by
construction. Within a token the id is a random 52-bit nonce — wide enough (and exact
through the float64 `cache.read`) that a same-token clash (an intra-tenant accuracy slip,
never a disclosure) is vanishingly improbable. The id therefore needs only per-token
uniqueness, never global.

The tagging is **lindy policy, not understudy behavior** — a standalone proxy user must
not have their `usage` rewritten. understudy exposes only a generic, optional
**response interceptor** (`Intercept(ctx, served, resp *http.Response) error`): a
lindy-injected collaborator that may rewrite the response — headers, body, or status —
in place before relay. `nil` (the standalone default) is pure passthrough, byte-for-byte
unchanged. understudy strips sensitive headers from the **upstream** response **before**
invoking the interceptor — so the strip sanitizes the backend's headers, but a header
the trusted interceptor deliberately sets (e.g. custom auth) survives — and **drops
`Content-Length`** when one ran (the body may
have changed length; the server re-frames — chunked under h1, `END_STREAM` under h2 —
and a stale length would corrupt the response); the interceptor owns any
`Content-Encoding` consistency, and — if it swaps `resp.Body` — owns closing
through to the original upstream body, since understudy releases the upstream
connection by closing `resp.Body` after relay. lindy's interceptor performs the id/`cache.read` rewrite
and records the provenance into the **tenant's own stream**: the shared daemon
(§Shared daemon) owns one stream per registered token — minted with the session, dropped
on teardown — and the interceptor routes each record by the token in `served`. lindy
serves that stream as a **sibling route on the daemon's mux** (`GET /requestlog`), gated
by the tenant's own bearer token (the same one that selects its data-plane config), so a
tenant reads only its own records. understudy never learns provenance exists — it gains a
mechanism (intervene on a response), not the policy. The interceptor's `served`
argument carries the target that actually handled the request — its `(backend, model)`
even after a failover, plus the requested model and token — so lindy's interceptor
attributes the response to what truly ran and files it under the right tenant. The empirical basis — which
fields survive opencode, and why the model-identity channels cannot — is
[notes/2026-07-12-served-model-provenance-join-key.md](notes/2026-07-12-served-model-provenance-join-key.md).

The orchestrator-side selection that *composes* a logical model's membership —
calibration, cost/quality tradeoffs, escalation — is future work, not covered here.

**Handler boundary: the mount emits; each side owns its own log record.**
<a id="handler-boundary"></a> understudy's `New` returns an `http.Handler` a
composition root mounts. The boundary splits three ways:

- **Emitting the request log is the mount's**, not understudy's — the mount emits one
  structured entry per request, so every route it serves (a daemon's `/session` control
  plane and understudy's `/v1` data plane alike) is logged, rather than understudy
  self-logging its own subtree while the control routes go dark.
- **Error rendering is understudy's** — the OpenAI-compatible error envelope is part of
  its protocol identity: the envelope `type`, the 5xx/401/403 body obfuscation (so
  internal detail and *which* auth check failed never leak to the client), and the
  distinct `retry_after_ms` reject shape. This is **not** caller-configurable: no mount
  wants a different wire format (a daemon's `{error}` shape is for its `/session`
  routes, which are not understudy handlers), and making it swappable would let a buggy
  caller break the contract. The status is read from the error itself
  (`interface{ HTTPStatus() int }`, with a status-less `context.Canceled` /
  `DeadlineExceeded` normalized to 499 / 504).
- **Populating the `/v1` request's telemetry is understudy's** — the per-request facts a
  mount can't see (which backend and model served, the upstream status, and on failure the
  *real* error behind an obfuscated body) understudy records into its own log record.

**understudy owns its telemetry record; what a consumer does with it is the consumer's.**
understudy's telemetry is understudy's, so its log record — the **`LogRecord`** value type —
lives in understudy and holds **only what understudy can supply**: which backend and model
served, the upstream status, and the real error behind an obfuscated body. Generic HTTP facts —
response status, byte counts — are deliberately **not** on it: they are not understudy-specific,
so putting them here would be understudy laying claim to facts that aren't its to own. A consumer
installs a record with `WithLogCtx` (which derives and returns the context), understudy's
handlers populate it in place, and the consumer reads it back as a **value copy** with
`LogRecordFromContext` (which reports presence). understudy neither emits a log line nor assumes a
consumer exists — with no record installed the `setLog*` populators no-op and
`LogRecordFromContext` returns the zero `LogRecord` and `false`; what a consumer does with that
(log a basic line, ignore it, nothing) is outside understudy's contract. understudy exports a
**read-facing** surface only — `WithLogCtx`, `LogRecordFromContext`, and the `LogRecord` type —
while the populators stay unexported. Write-locality does *not* rest on hiding the type: the live
record is a `*LogRecord` behind an unexported context key that only the unexported `logCtxFrom`
unwraps, and the read accessor hands back a value copy, never the pointer — so a consumer reads a
frozen copy, understudy writes, and nothing outside understudy can reach the live record to write
it (or even hold it).

The daemon's control plane keeps its **own** record for the one thing it logs — a
`/session` rejection's reason — and the mount reads *that* to emit the control-route entry.
That error never enters understudy's record: a control-plane rejection is not understudy's
concern, and merging the two would drag not-understudy business into understudy — the very
thing the split exists to prevent. So there are **two records**, each owned by the side
whose concern it is; the mount reads whichever the served route populated and emits one
entry.

This log record is understudy's own, distinct from the served-model provenance
`RequestMetadata` (§Served-model provenance): provenance is a per-request broadcast for
downstream joins/cost, while the `LogRecord` snapshot is what the mount projects to the log
line. They overlap on backend/model but answer to different consumers, so they stay separate.

## Concurrency & Rate Limiting <a id="concurrency-rate-limiting"></a>

understudy bounds the requests it holds in flight to one upstream account, so a burst
cannot trip the account's *own* concurrency limiting. The bound is keyed on canonical
upstream identity (§Upstream-identity canonicalization) and, in the shared daemon, is
therefore global across tenants (§Shared understudy daemon).

**The cap is learned, not configured.** An account's real concurrency limit is rarely
documented and varies by plan, so understudy estimates it from the traffic it is already
sending. A configured number would be wrong on every account but the one it was tuned
for. The configured value is a **cold-start allowance** only — where the estimate begins
before any evidence exists.

**Growth is demand-gated.** The cap rises only when a request actually waited for a
slot. Without the gate the estimate rises on every success, including on an idle system
where nothing is contending, and so encodes *cumulative successful traffic* rather than
capacity — it drifts upward without bound and the only thing that ever pulls it back is
provoking a rejection. Demand is what makes an increase evidence-bearing: raising a cap
nobody is pressing against asserts nothing about the account.

**Two growth regimes, split at the last known-good cap.** An increase of one slot per
successful request is geometric in aggregate, not linear: while saturated, a cap of *L*
completes *L* requests per round trip, so the cap doubles each round. That is the right
speed when the estimate is far below the truth and the wrong speed when it is near it —
approaching the real limit at doubling speed guarantees overshoot, and the overshoot is
paid for in real rejections. So the cap remembers the last value known to be safe and
changes instrument there:

- **Below it — one slot per success.** Doubling per round; the estimate is far from the
  edge and the cost of being wrong is one rejection.
- **At or above it — one slot per round.** Additive probing; the estimate is near the
  edge and each step must be cheap to retract.

**A rejection is a measurement, not a reflex.** A signal-less 429 arriving while
saturated says the account's limit is approximately the in-flight count at that moment —
the most precise capacity reading understudy ever gets. It sets the cap just below that
count and records it as the new known-good boundary. Halving instead would discard the
measurement just paid for.

Multiplicative decrease is held in reserve for the case that earns it: a repeat
rejection at or below the known-good boundary, which means the estimate is wrong or the
account is not understudy's alone. Halving is a *fairness* instrument — it is what makes
many independent flows converge on a shared resource — and the shared daemon exists
precisely so that understudy is the single flow per account (§Shared understudy daemon).
With no competing flow to converge with, halving on first contact costs accuracy and buys
nothing.

A 429 that carries a usable signal is not a concurrency measurement at all: it is quota
or rate exhaustion, and it routes to quota-class-aware demotion (§Understudy) rather than
to the cap. Only the signal-less case (a per-host fallback — [[understudy-ratelimit-signal-classifier]])
feeds the estimator.

**Per-upstream state outlives the tenant that taught it.** The learned cap is a property
of the account, accumulated across runs; tearing down a tenant frees its in-flight slots
but never resets the estimate (§Shared understudy daemon, Two lifecycles).

**The FD budget is a process backstop, not the account bound.** A process-wide limit
sized from `RLIMIT_NOFILE` sheds with `503` rather than queueing, so exhaustion cannot
accumulate the goroutines and buffered bodies it exists to protect. It guards the
*process* against resource exhaustion; on a host with a generous soft limit it sits far
above any cap the control law reaches and so bounds nothing about the account. Growth is
bounded by the demand gate and the known-good boundary, never by the FD budget.

## Shared understudy daemon <a id="shared-daemon"></a>

A single understudy process hosts many concurrent runs' configs, so a provider
account's concurrency cap and learned rate-limit state are enforced **globally**
rather than per-run. A per-process understudy sees only its own traffic, so N
concurrent `lindy run`s share an account's limit as N uncoordinated AIMD flows that
converge only approximately (TCP-style); one shared understudy sees the true
aggregate and enforces a real ceiling. The daemon **is the `lindy proxy` composition
root** (free tier) or understudy **multiplexed onto `lindyd`'s control-plane
listener** (paid) — an existing binary (§Binaries in DESIGN.md), not a new package.
The concurrency limiter is reused wholesale: the daemon is understudy run once, so
its in-process cap+queue+AIMD (keyed by upstream identity) becomes global by
consolidating all traffic into one process — no shared state store.

**Control plane.** <a id="daemon-control-plane"></a> The data plane is `/v1/*`; the
daemon adds a control plane. A run POSTs its resolved understudy config (backends +
credentials + models — what `Config.Resolve` consumes) to `POST /session`;
creating the session is the validation gate — the daemon resolves and validates it,
mints a token (`crypto/rand.Text()`), and returns `{token}`. The client already trusts
the daemon's CA — from the rendezvous file (the daemon publishes its details there
before signaling readiness), which it needs to make the HTTPS call at all — and points opencode at
`https://localhost:<port>/v1` with the token as bearer. **The token is the routing
key**: one shared `/v1/*` serves every tenant
and the bearer selects the config (unknown token → 401) — no path namespace. A run
**deletes its session** on completion with `DELETE /session` bearing its token; the
token is its own authorization (holding it already grants full data-plane access, so
dropping it needs no separate credential), and the delete is idempotent.

Every response the daemon serves — both planes — is logged by the mount (§Handler
boundary), so a rejected registration is as visible to an operator as a data-plane error.
On the data plane understudy populates its own log record (served backend/model,
upstream status, and on failure the real error) which the mount reads back as a `LogRecord`
snapshot to emit the `/v1` entry; on the control plane the daemon records only a `/session`
rejection's reason into its own record — the two never mix.

**Control-credential auth.** <a id="daemon-control-plane-auth"></a> The control plane
holds every session's upstream credentials in memory, so the threat is unauthorized
use of understudy **as a proxy** — an open relay once it is TCP-hosted. `POST
/session` therefore authenticates the registrant at the **application layer** with a
control credential it presents (the `X-Understudy-Control` header), never by transport
reachability (a loopback port isn't UID-gated; a same-user socket's protection
vanishes once TCP-hosted). The credential is delivered locally via the
`$HOME`-permissioned rendezvous file and operator-configured when TCP-hosted. Control
and data planes share **one loopback listener**: the credential guards `POST /session`;
per-run bearer tokens guard both `DELETE /session` and `/v1/*` (holding the token
already grants full data-plane access), so no separate socket is needed.

**Two lifecycles.** A **tenant** (a registered token → config) is ephemeral — one per
run, dropped on teardown or idle. **Per-upstream state outlives it:** the learned
concurrency cap + health for an account (keyed by upstream identity) is a property of
the *account*, accumulated across runs, so tearing down a tenant frees its in-flight
slots but MUST NOT reset the learned cap.

**Idle eviction.** <a id="daemon-idle-eviction"></a> Explicit deregister is the
primary reclamation path; an idle timeout is the crash-safety fallback for a run that
dies without deregistering. A tenant unused past a window — **minutes**, exceeding the
longest legitimate inter-beat gap (near opencode's ~10-min idle watchdog) so it never
rejects a live run mid-flight — is dropped. Each tenant carries its own idle timer
(`time.AfterFunc`), armed at registration and reset on every `Validate` under the
registry mutex, so eviction is intrinsic to the registry — no external sweeper for a
caller to wire or forget. A `Reset` racing an already-fired timer can drop a just-used
tenant, but the only effect is a correct 401 on its next request, and the window
(exceeding opencode's watchdog) keeps a live run from ever reaching that edge
([notes](notes/2026-07-20-idle-eviction-timer-over-sweep.md)).

**Upstream-identity canonicalization.** <a id="upstream-identity-canonicalization"></a>
Logically-same upstreams written differently across tenant configs coalesce onto one
limiter/health entry, or the global cap fragments per-tenant — the very problem the
daemon exists to solve. The concurrency limiter keys on a canonical `(base URL +
credential)` (`canonicalUpstreamKey`). Failover **health** keys on the canonical
`(url + key + model)` — `model` stays in the key so a per-model failure does not
demote sibling models on the same account
([notes](notes/2026-07-13-health-identity-granularity.md)).

**Discovery, fallback, version skew.** <a id="daemon-discovery"></a> Runs find the
daemon through a per-`$HOME` rendezvous file. **The daemon owns the file:** `lindy
proxy` writes it atomically (temp file + rename on the same filesystem, so a concurrent
discover reads the old or new record whole) once it is serving — *before* it emits its
readiness signal — and removes it on shutdown, so the file's lifetime tracks the process
it describes. The file is the **single source of the daemon's details** (address,
CA, control credential, version, and the daemon's PID — the last used only for stopping
it, below): a run that spawns its own daemon reads the file the same way a later
discoverer does. The daemon's stdout is only a **readiness/liveness
signal** to the run that spawned it — a line that arrives once the daemon is serving and
published, and a pipe that EOFs if it dies first — never a second copy of the
details.

**One daemon per `$HOME`.** `lindy run` shares the running daemon when one is live and
version-compatible, and spawns the daemon itself when it cannot see a live one — no file,
an unreadable/corrupt file (the daemon publishes atomically, so a malformed record is
never a live daemon's and is treated as dead), or a file pointing at a stale/dead one. It
never runs a *second* daemon alongside a first it can see. Two live daemons it *can* see
are fatal, not fall-backs: a version-**incompatible** daemon stops the run with an error
telling the user to stop it and retry (version skew is rare — the compat version tracks
config-format/capabilities, not build identity — so this is an occasional explicit stop,
not per-rebuild friction), and — the should-never-happen case — a live, cert-valid,
version-compatible daemon that *refuses* the run's registration also stops the run: it
published the very credential the run presents, so a refusal signals something wrong, and
the run fails loudly rather than masking it by cloning the daemon.

**Stopping the daemon.** `lindy proxy` is an ordinary daemon — it shuts down on SIGTERM
(as on `ctx` cancellation), removing its rendezvous file. `lindy proxy stop` reads the
PID from the rendezvous and sends SIGTERM, first confirming the PID is a live `lindy
proxy` so a stale file can't target an unrelated process. This is how the user resolves
the version conflict above.

**The file is a hint, not a trust root.** A run validates the daemon before handing it a
config (which carries upstream credentials):

- **Cert pinning.** The run dials the file's address over TLS pinned to the file's CA. A
  dead daemon refuses the connection; an unrelated process that reused the port fails the
  cert check. Either way the run falls back rather than leak its config to the wrong
  process.
- **Serialize spawn via an advisory lock.** `lindy run` takes an exclusive `flock` on a
  per-`$HOME` lock file around its discover-or-spawn decision, so two cold runs racing
  don't both spawn: the first spawns and publishes, the second wakes to find the published
  daemon and shares it — exactly one daemon, no clobber. The lock is a plain mutex, held
  only across that critical section; **staleness needs no lock** — a dead daemon is already
  caught by the register/cert-pin fallback (connection refused or cert mismatch → spawn).
  An advisory `flock` is used deliberately here — against the general preference to avoid
  them — because the kernel releases it on process death, so a run that crashes mid-spawn
  never wedges the lock.
- **Smuggled file.** The trust boundary for the file's *contents* is the filesystem: it
  is `0600` inside a `0700 ~/.lindy`, so only the user (or root) can forge it — and a
  same-user forger can already read the run's configured credentials directly. So cert
  pinning and the version handshake guard against an *accidental* mistarget, not a
  deliberate same-user forgery, which is an accepted boundary. When the daemon is
  TCP-hosted for multiple users, the application-level control credential (not the file)
  is the auth boundary.

A **version handshake** on discovery makes a version conflict **fatal**: the run stops
with an error and the user stops the incompatible daemon — lindy does not run a second
daemon alongside it. The compared version is not a
wire protocol — the `/v1` surface is opencode's (fixed, not ours) and the registration
format is incidental — but the daemon build's **config format and capabilities**:
whether it can correctly serve the config this run registers and offers the behavior the
run relies on. It bumps only when the proxy's config schema or feature set changes
incompatibly, *not* on build identity — the daemon is long-lived while `lindy` is
rebuilt beneath it, and most rebuilds touch neither — so a rebuilt run keeps sharing a
still-running older daemon whenever the contract matches, refusing only on genuine
incompatibility.

**Cross-tenant queue fairness.** Per-process, each run has its own cap so none starves
another; sharing one upstream's cap+queue across runs means one run flooding the queue
could starve another, so the shared queue needs a fairness policy (e.g. per-tenant fair
queuing) on the *same* queue.
