# kronk: upstream feedback

Running list of things to raise with kronk upstream. Findings are against the
v1.30.7 source; the box that produced them runs v1.30.3.

## What understudy depends on — please keep stable

`kronkpool.ErrServerBusy` → `503` with `code: "unavailable"`
(`cmd/server/app/sdk/errs/errs.go` `FromSDK`, pinned by `errs_test.go`). It is
the *only* producer of that code outside the auth middleware, which makes it a
precise signal that a model swap was refused because the pool entry it needed
was busy. Understudy keys on it to convert the refusal into a short client-side
retry instead of failing the request. A change to either the status or the code
string breaks that.

## 1. Eviction sheds immediately; admission waits

A request for a resident model queues (up to `AdmissionTimeout`, 3m default). A
request needing a *swap* is refused in under a second if the entry it would
evict is mid-stream. Same GPU, same contention, two policies — so `M1, M1, M1`
and `M1, M2, M1` behave completely differently.

`evictOneIdle` (`sdk/pool/engine/eviction.go`) scans once for an entry with
`ActiveStreams() == 0` and returns `ErrServerBusy` if none is idle *at that
instant*. The `pollInterval`/`maxWait` (25ms / 60s) constants declared at the
top of that same function are used only *after* a victim is chosen, to wait for
the eviction callback. So kronk will poll 60s for an eviction it has committed
to, and 0ms for one it hasn't.

**Repro:** start a long stream on model A, then request model B. Rejected in
~0.7–1.2s. Serially (pool idle) the same swap succeeds in ~9s, so the swap
itself is fine — only contention triggers it.

**Effect:** a concurrent multi-model workload fails deterministically rather than
slowly. Fanning out ~19 agents across two models that cannot co-reside (16GB
card), every agent on the model that loses the slot fails within a second.

Bounding the victim *search* the way the post-selection wait is already bounded
would make the two paths consistent. There may be a reason for the fast shed —
starvation, if same-model requests keep arriving — but that risk applies to the
committed wait too. Related comments at `sdk/kronk/concurrency.go:56`,
`model/chat.go:152`, and `model/batchgen_finish.go:619` suggest the single check
is deliberate, so this is a question rather than a bug report.

## 2. `RESOURCE_EXHAUSTED` → 429 loses two distinctions

`FromSDK` maps both to `ResourceExhausted` → HTTP 429:

- `kronkpool.ErrNoCapacity` — `"resman: insufficient memory budget"`. The model
  does not fit. A standing property of the box, not a transient one.
- `kronk.ErrAdmissionTimeout` — waited out the admission budget. Transient
  overload.

Two unrelated conditions under one code, then projected onto a status that fits
neither: HTTP 429 means *this client* is sending too fast, so it tells a caller
to slow down when slowing down cannot help. `ErrNoCapacity` in particular is a
server-side fault — kronk advertises the model in `/v1/models` and then cannot
load it — which reads as 5xx (503 if freeing other reservations could change the
answer, 500 if not). `ErrAdmissionTimeout` is closer to a genuine overload
signal, but 503 fits it better than 429 as well.

## 3. `Server: kronk/<version>` on every response (nice-to-have)

Nothing in a kronk response identifies kronk — no `Server`, no vendor header,
and headers are byte-identical across 200, 503, and `/v1/models`. The only
marker is `system_fingerprint: "fp_kronk"` in success bodies, absent from
errors.

A proxy fronting several OpenAI-compatible backends has to decide whether a
given 503 means "busy, retry" or "broken, fail over", and that answer is
server-specific. It can be configured when the operator knows what they pointed
at, but the operator who writes the URL is not always the one who chose the
server. A version header lets clients adapt without a config change, and must be
on error responses to help — that is the response being classified.
