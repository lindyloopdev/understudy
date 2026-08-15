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
  `401` is the diagnostic and reaches the client — the one exception to the
  access-refusal failover below, and that exception covers the `401` alone. A
  `401` here contradicts the *declaration*, not an account, so no sibling can
  serve in its place; routing around it would hide a wrong config behind a paid
  backend.
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
candidate list** (`BackendConfig.Models`) that understudy **fails over** across. A
request naming a `<backend>/<model>` reference instead resolves to that one target
and is not failed over — a logical model *is* the declaration of which substitutes
are acceptable and in what order, so a reference, declaring none, leaves understudy
nothing it is authorized to try. Both forms are otherwise identical: everything
below keys on the canonical endpoint, never on how a request spelled it.
understudy routes each request to the first target not currently
failing past a threshold, tracking a failing-since per canonical `(url + key + model)` and
classifying a 502/connection error as an availability failure. understudy **walks** the list,
advancing to the next target when an attempt is **classified an availability
failure** — a hard error (502/connection), a rate-limited target, or **refused
access**: a `401` (identity rejected), `402` (out of funds), or `403` (not
permitted). Refused access is an availability fact about the one account called,
not a client error — a cost-ordered candidate list reaches an exhausted balance in
the ordinary course, and an account is routinely entitled to one backend's model
and not another's — so the target is demoted at once and the request continues on
a sibling instead of the refusal reaching the client. The three differ only in why
the account may not use the target, never in what understudy can do about it
within the request.

A backend declaring `auth = none` is excluded **from the `401` arm only**: it
supplies no credential to refuse, so its `401` is a config diagnostic and passes
through (see §LLM API Keys via Understudy). That argument is about credentials, so
it does not reach `402` or `403`, which say nothing about a declaration.

**Every `403` is account-scoped, so this arm needs no request-scoped guard.** That
is a finding about what providers send, not an assumption drawn from the status. A
refusal the *request* earned arrives one of two ways: as a `4xx` naming the request
itself as the defect, or as a **successful** response the model declined, which
understudy relays untouched. Neither is classified here. A `403` means the account
may not use the target — an unsupported region, a permission its key lacks — and
even where a safety system answers `403`, it is aggregated over the account's
traffic and clears on its own, which is account-scoped and temporary: exactly what
demotion and recovery already handle.

The guard is therefore unbuilt on purpose, and the damage it would prevent is
unreachable until that finding stops holding. Were a provider to answer `403` for
one request's content, understudy would replay it across every candidate, be
refused identically at each, and demote them all on the way — benching targets
that serve other requests fine, on the strength of one request. That is the signal
to stop the walk and keep such a refusal out of the health map, since a fact about
one request is not evidence about an account. Until a provider sends that shape,
there is nothing for a guard to distinguish.

**Least degradation: a backend understudy cannot use costs that backend, not the
request.** The refused-access rule generalizes past runtime faults. A backend
may also be unusable *statically* — its `provider_type` has no registered handler,
or it reached understudy without a base URL — and that is still a fact about one
backend, which the candidate list exists precisely so one fact cannot decide the
request. So an unusable backend is **skipped where a target is chosen**, and a
request that resolves to a usable sibling never pays for it.

Skipped, not demoted. Refused access is a fact about the world that can
change on its own, so it belongs in the health state the failover walk consults; a
statically unusable backend cannot become usable without a configuration change, so
demoting it would seed a health entry no recovery probe can ever clear
(§Recovery probing). Rejecting up front is the other wrong answer: validating every
declared backend before routing buys deterministic error ordering at the cost of
turning one typo into total unavailability, failing requests that would never have
touched the offending backend. Determinism is recovered by validating the backend
actually chosen, where it is chosen.

**The reason travels with the skip.** A skip that discards *why* turns understudy's
own errors into falsehoods — a caller told a configured-but-unusable backend is
"unknown", or told "no backend configured" when several are. So every reason reaches
the operator, through the request's `Excluded`. It does not reach the client: a
caller told a backend "must provide base_url" has been handed a diagnostic it cannot
act on about a deployment it never named, so it is answered in understudy's own
words instead.

**A list emptied by misconfiguration answers 404, not 500.** The two ways a
candidate list runs out are not the same ending. Emptied by *health*, something is
still worth attempting — a demoted target can serve again — so the request is made
and the client receives the upstream's own answer, or the ladder's reject once the
streak passes the terminal threshold (§Understudy, the Retry-After ladder). Emptied
by *static unusability*, there is nothing to attempt: no upstream is called, and the
client is told the model has nothing to serve it, while why each backend was skipped
goes to the operator.

That answer is a 404 even though the fault is the operator's, for three reasons.
Least degradation already holds that an unusable backend costs that backend and not
the request, so faults do not sum: if one misconfigured target still serves a
request from a healthy sibling, three misconfigured targets are still only facts
about themselves, and what decides the request is that nothing is left. Second, a
5xx body is rewritten to its bare status text, so a 500 would discard the very
reason the paragraph above requires to travel — 404 is the only status on which the
requirement is satisfiable. Third, a model that cannot be served is the condition
`invalid_request_error` already names, whatever put it in that state.

The rule is about *usable targets*, not about how the list came to be empty. A
logical model declaring no targets at all is the same condition as one whose every
target is unusable, and answers the same way, in the same words.

**Operator and caller learn different things.** The caller gets the best available
answer to the request it made; the reason a backend was skipped is the *operator's*
fact, and it reaches the operator through the request's `LogRecord` rather than
understudy's own logger. In an embedded deployment the two may be the same person;
understudy must not assume it, so a caller's request is never failed to deliver an
operator's diagnostic.

Reporting the skip rather than logging it is what keeps understudy out of a policy
it has no standing to set. A `TokenValidator` runs on every request, so a skip
recurs on every request, and suppressing the repetition means choosing a unit to
suppress it over — a unit understudy cannot see. Per process is wrong for a proxy
that runs for weeks; per token assumes a token lifetime understudy is not told and
which is static in the ordinary case; per elapsed interval is a number invented to
stand in for a cadence the consumer knows and understudy does not. The consumer
knows its own unit of work, so the fact is handed to it and it decides whether that
becomes a log line, a metric, or nothing.

**A target's health transitions are understudy's own to log.** The rule above sends
an operator's facts through the request's `LogRecord`, because understudy cannot know
the unit over which a recurring fact should be suppressed. A health transition is the
exception, and the reason is that it does not recur: it is an edge, and the unit is
understudy's own — a failure streak it began and will end. Suppressing a transition
per streak invents nothing about the consumer's work.

The other half of the reason is that a recovery has no request to ride on. A target
coming back excludes nothing and fails nothing; no walk moves past it, so `Excluded`
cannot carry it, and a consumer assembling health from request records alone would
watch targets go down and never come back — reading a permanently degraded fleet.
Degradations are worth knowing, and knowing them without recoveries is worse than
knowing neither. Both directions are logged, or neither is.

The exception is narrow. It covers the transition — this target is out, this target
is back — and not the per-request fact that a walk routed around it. That fact is a
skip like any other and belongs on `Excluded`, on every request that routes around
the target, exactly as the rule above requires.

**The pair is not ordered.** Two requests can decide a target's transitions in quick
succession — one demoting it, another finding it healthy — and each writes its record
after releasing the lock, so an operator can read "backend up" before the "backend
down" it followed. What is ordered is the decision: `downLogged` is claimed under the
lock, so exactly one record is written per streak and the pair is never doubled or
dropped. Ordering the writes too would mean one serialization point outside the lock,
paid on every transition to correct a reading that the next event corrects anyway.

**A transition is never emitted while the health map is held.** The record is decided
under the lock and written after it, because the handler belongs to the consumer and
may be slow: a file, a socket, a shipper with a full buffer. Every request's routing
reads that map, so a handler run under it would hold the whole proxy for as long as it
took to write one line — and a backend going down is exactly when requests arrive
together. Deciding and emitting are therefore separate steps in every path that
reports one.

**A transition is logged when it happens.** The demotion is the event: the request
that was refused, stalled, or told to back off holds the failure, knows what the
upstream answered, and knows the moment. Logging instead at the next walk that routes
around the target dates the record to when someone tripped over it, drops the cause,
and says nothing at all when no one trips. What such a record owes an operator — the
target, why it is out, when it will be tried again, and whether that moment is the
upstream's terms or a bench understudy chose — is in scope at the demotion and none
of it is at the walk.

One shape has no such moment. A target whose streak merely ages past the failover
threshold is routed around from the instant the threshold elapses, and nothing runs
then; the crossing is computed by whichever walk next weighs the streak against it.
Where a further failure does the crossing, that failure is the event. Where the
streak ages in silence, the walk that discovers it is the earliest knowable moment,
and the record answers for that by dating the streak rather than the discovery.

Everything a request moved on from goes on one record, `Excluded`, whatever kept it
from serving: a target abandoned after a call, a target understudy could not use and
never called, a backend a listing left out. The candidate that *ended* a walk is
carried in the record's own fields when it is what the request answered from; when
an earlier candidate answers for the request instead, it joins `Excluded` like any
other the walk moved past. They differ
in why they did not serve, not in what the consumer is being told, and one list
preserves the order a request walked its candidates in — an exclusion and a failover
interleave, and two lists could not say which came first. `Called` carries the
distinction the shape no longer does: whether understudy sent anything before
excluding it.

Order is only meaningful where a walk produced it. A chat request walks an ordered
candidate list; a listing ranges a map, so its exclusions arrive in whatever order
the range yields and none is implied.

**Two endpoints, two answers.** `/v1/models` asks *what can you serve* — emptiness
is a valid answer whatever its cause, so unusable backends are skipped and a usable
backend offering no models contributes nothing; the listing does not fail because
some backend, or every backend, could not be reached. Chat asks understudy to
*serve this*, and there failure is failure: a request naming a model no usable
backend can serve is an error, and why each backend was skipped goes to the
operator with it. The consequence is deliberate —
a total upstream outage renders as an empty catalog rather than an error, and the
operator learns of it from the record rather than the response.

A consumer wanting stricter behavior enforces it in its own `TokenValidator`,
before handing understudy a configuration. Routability as *understudy* defines it
(a registered `provider_type`) is not visible from there; exposing the registered
set would make it so, and is deliberately not built until a consumer needs it.

**Stalls: two axes, three dispositions.** Whether a stalled request can be
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

**A success clears the streak, so only a target failing outright is routed
around.** Any success on an account deletes its failure record, so crossing the
threshold takes a window in which *nothing* succeeded. A target failing a fraction
of its requests keeps receiving them, and how fast one demotes depends on how busy
the account is — the volume dependence the duration threshold otherwise avoids.
That is accepted, not overlooked: a partly-serving target still beats no target,
and every rule for netting failures against successes needs a window and a ratio,
the tuning constants these thresholds exist to do without until cost can derive
them. Revisit when a partly-failing target is seen hurting a run, which is the
evidence this trade is missing.

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
rules** — the `thinking` value's semantics — are validated wherever a reference
is accepted, never in the decode: at `Config.Resolve` beside the existing
backend-reference check, and again when a request names a `backend/model`
reference directly, which is a target the caller wrote rather than the operator.
One reference means one thing however it arrives, so an override the config would
refuse is refused from a request too — as a `400`, since there it is the caller's
input that is wrong. An **unrecognized query param is ignored, not rejected**
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

**understudy never picks a model, and reserves no model name.** Every logical
model is one the operator declared, and a request resolves to a declared target or
it fails; there is no name understudy treats specially and no catalog it consults
to invent one. An OpenAI-compatible `/v1/models` is a flat list of ids carrying no
capability metadata — embedding, audio, and image models sit in it beside the chat
models — so an inferred pick can land on a model that cannot serve a chat
completion at all, and understudy has no way to tell. Choosing between them is the
quality judgment **Availability, not quality** reserves for the orchestrator, and
inferring one also spends a live catalog fetch on the request path merely to
resolve a name. Which of its logical models a consumer requests is the consumer's
concern, not understudy's.

This is what makes the configless guarantee structural rather than a rule to
enforce: routing reaches only declared targets, so a configured understudy has
nowhere to silently fall back *to* — the budget caps, audit, kill switch, and
egress control an operator is paying for cannot be traded away by a resolution
understudy performs. Ease of first run is a **packaging** concern, answered by a
shipped example configuration that declares its own models, not by understudy
guessing.

**Rate-limit reject.** A long upstream `Retry-After` on a retryable failure — a
`429`, or a `5xx` outside the never-retryable class — is converted to a
non-retryable **400** before the agent sees it — opencode honors `Retry-After`
with no ceiling (~24.8 days) and lindy can't detect the situation from the event
stream, so the reject must live in the proxy. The response splits status (400 =
retry-control for the agent) from envelope `type` (the reason for lindy, e.g.
`upstream_rate_limited`).

**Synthesized backoff (no `Retry-After`).** The reject's converse: a *retryable*
failure carrying **no** `Retry-After` — a 429 without the header, or a 5xx — is
not relayed raw either. Unhandled, opencode hammers rapid retries at a failing
upstream (or, on its unbounded path, hangs). understudy instead **synthesizes** a
`Retry-After` and injects it while preserving the retryable status, so a client
backs off *understudy's* interval instead of its own. opencode's agent turn is
not such a client — it calls the SDK with `maxRetries: 0` and makes one attempt,
so nothing injected reaches it — [[fail-over-from-a-bare-429]]. Only the `429` half is
built: a `5xx` with no delay still reaches the client bare, and the injected
interval is a fixed constant — [[understudy-adaptive-coordinated-backoff]]. The
interval grows exponentially per backend, is jittered, and resets on success; its ceiling **is
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

**Rate-limit demotion is quota-class-aware.** The delay a 429 sends is not
always honest about when the target recovers, so demotion keys on the *quota
class*, read from the structured `QuotaFailure.quotaId` in the body (Gemini's
OpenAI-compat 429 carries the native `google.rpc` details — see
[notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md](notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md)).
A **per-day** exhaustion (`…PerDay…`, e.g. free-tier RPD) sends a misleading
sub-minute `retryDelay` yet does not reset until a fixed daily boundary (midnight
Pacific for Gemini), so understudy benches the target until *that boundary*, not
the delay it sent — instead of re-probing every ~40s into a wall it can't
clear for hours. A **per-minute** throttle (`…PerMinute…`, RPM/TPM) keeps its
short delay: that window really does clear in seconds. The delay the upstream
sent stays the demotion input everywhere else; the quota-class override applies
only where that delay provably lies. (The delay itself is still read from message
*prose* — the structured `retryDelay` is present but has flipped in and out across
endpoint changes, so prose remains the resilient source; the quota id is the one
structured field we depend on.)

**Recovery probing is demand-triggered and off the request path.**
<a id="recovery-probing"></a> A demoted target is re-admitted by a half-open
probe, and that probe is neither carried by a client request nor driven by a
standing timer. A request arriving for a logical model whose candidate list holds
a target due for a check is **served immediately by the first healthy target**,
and the probe is launched **asynchronously**; its outcome lands in the health map
for the next request to read. Demand is the trigger, so an idle install — a
single user away for a long weekend — issues nothing at all; but no client ever
pays the probe's latency, which for a stalled target is the full header-stall gate
and for a slow 5xx is worse.

Both alternatives are rejected. A **standing background timer** polls on a
schedule set by the outage's length rather than by anyone's need, so its cost
grows while nobody is waiting — wrong for the single-user installation understudy
is first deployed into. **Probing in-band** — routing a live request to a target
believed dead, as a demoted target's next caller — spends a client's latency, and
sometimes a hard failure, on a target the failover list can already route around.
A middle option, proactive polling gated on *recent activity*, is also rejected:
during active use the traffic already is the clock (a beat's requests arrive
seconds apart, so a staleness check on arrival fires about as often as a timer
would), and where traffic is sparse the lag costs one request served by a fallback
rather than the preferred target — a cost-order regression, not an outage. Not
worth a timer, a lifecycle, and an activity window.

The probe is a **synthetic minimal completion**, never the triggering client's
body: that payload is the client's own data, and replaying it bills tokens for a
response nobody reads. It must exercise the **chat** path, the only one that tests
the same credential, balance, and quota bucket a real request does — `/v1/models`
shares neither the billing check nor the quota. A probe against a target still
down bills nothing (a 429, 402, or 5xx is free), so the probe's cost is paid only
on the success it is looking for.

**The schedule escalates with the outage's age, jittered, and capped.** The
interval grows from `failingSince` — the hazard of recovery declines with how long
the outage has run, not with how many times understudy happened to look, so age is
the honest independent variable, and it is the one the failover and terminal
thresholds already key on. It is **capped low** (≈15m): at that rate a dead target
costs under a hundred free probes a day, so growth past it buys rounding error
while making worst-case detection unbounded — worst precisely when an operator has
just paid and expects work to resume. Jitter, for the same reason the synthesized
backoff needs it; reset to base on success. A known `readmitAt` **supersedes the
schedule entirely** — there is nothing to discover, the upstream's time is the
answer. A success on a concurrent request deletes that time along with
the streak today — [[a-success-clears-more-than-its-own-streak]].

**Health belongs to the endpoint, not to the route that reached it.** The key is
the canonical `(url + key + model)` and nothing else — not the logical model walked
to get there, not whether the caller named a logical model at all. A request naming
`<backend>/<model>` directly reaches the same upstream as a logical model whose
target resolves to it, so it reads and writes the same entry: what one learns about
an account, all learn. Keying health on how a request was *addressed* would make an
account's availability depend on spelling, and let a demoted upstream keep serving
one route while another routes around it.

**Credential rotation needs no probe.** Health is keyed on the canonical
`(url + key + model)`, so a rotated key is a *different* entry, healthy by
construction: the demotion stays pinned to the credential that actually failed and
the new one is live on its first request. The schedule therefore covers only
*same-credential* recovery — an un-suspended account, a propagation delay, a
topped-up balance (`402`, which no rotation heals and which the operator expects to
clear the moment they pay).

Two consequences to build against. understudy **originates traffic it was not
asked to send**, the same charter expansion the synthesized backoff makes — pure
relay toward controller — and is bounded the same way: one probe in flight per
target, only while that target sits in a live candidate list. And a probe
**outlives the request that triggered it**, so it cannot borrow that request's
context or credential lifetime; it needs a server-scoped lifetime, a copy of the
config taken at trigger, and single-flight so concurrent arrivals launch one probe,
not many. An explicit operator signal (a "recheck now" on the daemon's control
plane, §Control plane) beats any schedule for the case the operator can see
coming; the schedule is the unattended fallback.

**Request disposition: a Retry-After ladder.** Given a retryable failure and a
healthy next target, understudy's disposition is gated by the remaining
`Retry-After` — where the upstream sent one — against a **wait budget** — the
tolerable in-request delay before switching, bounded by the client's own timeout
and widened by the cost of the cheapest fallback (a dear fallback is worth waiting
longer for):

- **No delay sent → fail over.** A bare 429 gives the budget nothing to weigh:
  understudy cannot know the wait is short, and the client will not wait out a
  delay nobody gave it — opencode's agent turn makes a single attempt
  (*Synthesized backoff* above). So the request goes to the next untried target
  rather than becoming an error the caller cannot act on. Demotion stays the separate
  question: a bare 429 is a capacity measurement (§Concurrency & Rate Limiting),
  so the target keeps its place in the walk. A 429 that *did* name a short delay
  is the open half — [[fail-over-from-a-bare-429]].
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
  key is **inferred from the payload and scoped to the token**: a hash of the
  invariant leading messages (system + first user turn), which is also exactly
  what prompt-cache coherence keys on, mixed with the bearer token so one
  tenant's affinity cannot steer another's routing in the shared daemon. The
  token is hashed, never stored raw, so understudy's own state cannot be read
  back into a credential.

  **Affinity engages only on a request carrying a prior assistant turn.** A first
  turn has nothing to stay coherent with, so it takes the walk as ordered — which
  is what keeps affinity from competing with the walk's own decisions, since a
  within-threshold target and a probe-due one are both reached by first turns. A
  first turn still *records* the target its later turns prefer.

  **Affinity is a short-lived hint, not a lease.** No end-of-conversation signal
  reaches the wire, so it is refreshed by use and expires on idle, its lifetime
  tracking the provider's prefix cache: once that is cold, staying buys nothing.
  Turns are seconds apart and runs minutes or hours, so idle expiry scopes
  affinity to an active conversation without understudy knowing what a
  conversation is — and bounds the map, an idle record being worthless by
  construction rather than merely old.

  **Compaction releases affinity, and should.** A compacted session continues as a
  summary plus a recent window, so the first user turn stops being sent and the key
  changes — the mechanism agreeing with the policy, since the cache it preserved is
  cold on every target at that moment, making compaction the cheapest point to
  re-balance. A caller-supplied identifier would hold on past it and need an
  explicit release to match. What compaction does *not* clear is wire-format
  compatibility: the preserved window still carries turns shaped for the target
  that authored them ([[keep-a-conversation-on-one-thinking-mode]]).

  **Affinity is tenant state, not account state.** Health and the concurrency cap
  describe an upstream account and outlive the tenant that taught them; affinity
  describes a caller's conversation, so it goes when that tenant is deregistered or
  idles out (§Shared understudy daemon, *Two lifecycles*). It is therefore grouped
  per tenant rather than keyed on it — the conversation key says which conversation,
  the group says whose — which is what lets a teardown find them. Until a registry
  exists to call it, the idle TTL is the only reclamation.

  **Concurrency on a key is the tell that it has collided.** A conversation is
  serial: the caller cannot send a turn until the last one answers. So a key with
  two requests in flight at once is not one conversation, whatever caused it, and
  the later arrival takes the normal walk. This is what keeps the mechanism
  degrading quietly rather than wrongly, because its discrimination rests on
  caller properties understudy cannot see: distinct charters per reviewer, and
  distinguishing content in the first user turn rather than in a tool result.

  **Feasibility-spike first** — confirm the leading-prefix hash stays stable
  across a real session's turns before building on it. Read from the shipped
  opencode bundle (2026-08-14): compaction is a session message carrying a
  summary, cut at a user-message boundary against a `preserve_recent_tokens`
  budget. Still unconfirmed on the wire: that the preserved window carries raw
  tool-call turns, and what the `ModelSwitched`/`AgentSwitched` message types mean
  for a conversation whose model the caller changes itself.
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

**One table maps an upstream failure to what the client is told.** A provider reports
what the upstream *said* — the condition it observed, and any retry boundary it
recovered — and understudy alone derives the status, envelope `type`, and
`Retry-After` from it. Relaying a provider's own type string would make understudy's
contract the union of every vendor's vocabulary and leak a name one vendor invented
(`RegionError`) into the field a consumer dispatches on; it would also misdescribe a
response understudy has already reshaped, since by the time a client sees an error the
status may be normalized, the body obfuscated, the backoff synthesized, and the target
a different one than was asked for. The table is **documented for library users**, not
left to be read out of the code, because a consumer classifies on it. It is not
caller-overridable, for the same reason error rendering is not: a hook would let a
caller break the contract its consumers depend on. Should a real need for one appear,
it is a later addition, not a reason to build the seam now.

**Upstream failures.** Status is retry-control, `type` is the reason, and the body is
obfuscated for `5xx`/`401`/`403` per the rule above. These are a request's **final**
disposition: a refusal, a stall, and a `429` past the demotion threshold each fail
over first, so a client is told one of these only once no untried candidate remains.

A **`5xx` no retry can help** is `501`: the operation is not implemented, and no
delay changes that. It is a standing fact like a refusal, not the transient fault
the rest of the range describes. The tables below name the class rather than the
status, so a row reads as the rule it follows.

| what the upstream did | client status | envelope `type` | retry delay |
| --- | --- | --- | --- |
| `400`, `404` — the *request* is at fault | relayed unchanged | `invalid_request_error` | — |
| `401`, `402`, `403` — the account may not use this target | `400` | `upstream_refused` | none; the account's own recovery clears it |
| `429` with a delay under the demotion threshold (≈30s) — a throttle, retried in place | `429` | `rate_limit_error` | the delay still outstanding |
| `429` with a delay from that threshold to the passthrough ceiling (≈2m) — demotes the target | `429` | `rate_limit_error` | the delay still outstanding |
| `429` with a delay beyond the ceiling | `400` | `upstream_rate_limited` | `retry_after_ms` in the body |
| `429` with no delay | `429` | `rate_limit_error` | synthesized |
| a `5xx` no retry can help | `502` | `server_error` | none, whatever it sent |
| any other `5xx` with a delay up to the passthrough ceiling (≈2m) | `502` | `server_error` | the delay still outstanding |
| any other `5xx` with a delay beyond the ceiling | `400` | `upstream_unavailable` | `retry_after_ms` in the body |
| any other `5xx` with no delay, or a transport failure that never answered | `502` | `server_error` | synthesized — nothing is sent today, [[understudy-adaptive-coordinated-backoff]] |
| overloaded (`529` and kin) | `502` | *open* | as `5xx` |
| every candidate stalled before its header | `504` | `server_error` | — |
| failing past the terminal threshold, nowhere left | `400` | `upstream_unavailable` | `retry_after_ms` in the body |

**A walk that runs out of candidates answers for the request, not for its last
target.** Relaying the final failure makes the answer depend on list order: with
`[a: 429 for 60s, b: 401]` a caller would be told to escalate a standing refusal,
when `a` is merely throttled and will serve once its delay elapses. Only a sustained
or bare `429`, a refusal, a stall, or a history the target cannot accept replays at
all — a plain `5xx` ends the walk where it falls — so it would be the last candidate whose answer a client sees. The verdict is
instead the **most optimistic** disposition among the candidates the request had —
including one it declined to call, because a target benched until a known time is
as time-bound as a disposition gets.

Every candidate contributes a delay or contributes nothing, so there is no tie to
break by judgement:

| what the candidate did | contributes |
| --- | --- |
| sent a `Retry-After` | what remains of it |
| answered a retryable failure with no delay — a `429`, or a `500`/`502`/`503`/`504`/`529` | that endpoint's current synthesized interval — [[understudy-adaptive-coordinated-backoff]] |
| stalled before its header | the synthesized stall backoff |
| was benched and never called | its `readmitAt`, less now |
| refused — `401`, `402`, `403` | nothing; no delay it named, and no bench it earned |
| rejected the request's history | nothing; no delay will make it serve this conversation |
| answered a `5xx` no retry can help | nothing |
| was unusable as configured | nothing; only a config change clears it |

The verdict is the **soonest** contribution, answered in the shape of the candidate
that made it: a bench earned by a rate limit answers as a rate limit, carrying that
bench's remaining time. Answering in the shape of whichever failure happened to end
the walk would tell a client to stop and to retry in the same breath. A verdict with
no contribution at all is the stop. Not every row is weighed yet —
[[weigh-every-candidates-contribution]].

**A target that cannot serve *this* conversation is routed around, not adapted.**
DeepSeek requires `reasoning_content` on an assistant turn carrying `tool_calls`,
which a history authored under `?thinking=false` lacks. That says nothing about the
target's health, so the walk replays onto the next untried candidate and leaves its
health alone. It does not rewrite the body to suit: disabling a thinking mode the
operator configured substitutes a capability nobody declared, and that judgment
belongs to whoever composed the list ([[understudy-scope]]). The rejection recurs for
every request of that conversation, so it reaches the operator on `Excluded` like any
other skip, never understudy's own log.

Optimism is the cheaper error. Guessing retryable when nothing will recover costs
one more failed request; guessing terminal when something would have recovered
strands work that could have run. The individual reasons are not lost — every
candidate the walk moved on from is on `LogRecord.Excluded` with the status it
answered, and the one the request answered from is in the record's own fields,
which together are where an operator reads what actually happened.

A relayed failure carries its delay in a `Retry-After` **header** — retry-control
aimed at the agent, which sleeps and retries on its own. A **reject** carries it as
a top-level `retry_after_ms` (milliseconds, rounded to the second) beside the
`error` object, and sends no header at all. Both would reach lindy either way: the
agent publishes an error event carrying the response body *and* its headers. The
header is withheld because `Retry-After` **is** an instruction to sleep, and a
reject exists to make the agent stop — so the delay is put where only the
orchestrator above it will read it. understudy is speaking past the agent to
lindy, which schedules the retry itself.

An upstream's own `type` string is still relayed ahead of this table today, except
on the refusal row — [[understudy-error-envelope-type]].

**A failure the caller cannot act on says so and no more.** Where a request fails
on understudy's own arrangements with a provider — a credential rejected, a balance
spent, a resource the account was never granted — the client is told the request
cannot be served, and never which of those it was. It can pay no bill, rotate no
key, and opt no account into a region, so the cause is not help to it; and
understudy's client is a party the operator has already decided not to trust with
secrets (§LLM API Keys via Understudy), which makes the operator's commercial
standing not understudy's to disclose either. The cause goes to the log, where the
operator reads it. The envelope `type` still separates a refusal from an
outage, because a consumer has to tell *wait* from *escalate* — that much is
remediation, not narration.

**A `500` is understudy's own fault, never an upstream's.** Every upstream `5xx`
is flattened to `502` — which one a backend chose is not the client's business —
and `500` is reserved for understudy failing: a panic it recovered, a configuration
it cannot serve from. That reservation is why the flattening happens where a
failure is known to be a relay, rather than where every error is rendered: by then
understudy's own `500` and a backend's are indistinguishable, and the reservation
would be lost.

**understudy's own refusals**, which reach no upstream:

| condition | status | envelope `type` |
| --- | --- | --- |
| session token absent or rejected | `401` | `authentication_error` |
| model undeclared, backend unknown, or a model with no targets | `404` | `invalid_request_error` |
| malformed body, malformed reference, or a rejected override | `400` | `invalid_request_error` |
| client disconnected | `499` | — |
| request deadline exceeded | `504` | — |
| understudy panicked, or cannot serve from its own configuration | `500` | `server_error` |

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

**A rejection is a measurement, not a reflex.** A bare 429 arriving while
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
to the cap. Only the bare case (a per-host fallback — [[understudy-ratelimit-signal-classifier]])
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
