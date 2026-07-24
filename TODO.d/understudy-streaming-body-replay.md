# Stream the request body for within-request failover (replace the eager buffer)

**Tag:** understudy / fallback / performance

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

Within-request failover ([[understudy-fallback]]) currently reads the whole
request body into memory (`io.ReadAll`) before forwarding, so a replay to the
next target has the bytes. That is correct but eager. Replace it with a
**streaming** form: forward the first attempt as it arrives and only materialize
the full body when a failover actually needs it.

**Why:** chat-completions requests are a stream by nature and can be large;
buffering taxes memory and latency, and eagerly reading the client before
forwarding stalls a slow client — which bites hardest once understudy runs
cross-host from `lindyd` ([[understudy-shared-daemon-subserver]]). Streaming is
"make it fast" after "make it right"; the eager form ships first.

**The correctness constraint (do not skip):** the outbound transport and any
drain must never *share* `r.Body`. `io.TeeReader(r.Body, buf)` handed to the
transport, then draining `r.Body` on failover, is a **data race** — the
transport's writeLoop may still be reading `r.Body` after `Do` returns (writeLoop
and readLoop are separate goroutines; closing the *response* body does not
synchronize the *request* body). Intercepting the transport's `Close` fixes
lifecycle (don't let it close/lose the underlying reader) but does **not**
synchronize reads.

The sound design is a **sole-reader wrapper** that owns `r.Body`:

- `Read` under a mutex: read `r.Body`, tee into `buf`; return EOF once closed.
- `Close` (intercepted, never closes `r.Body`): flip `closed` under the mutex,
  signal a `done` channel.
- Failover drains the remainder only after `<-done` (select against `ctx.Done()`
  so a never-closed body can't hang), under the same mutex.

The mutex serializes `r.Body` access even if `Close` races an in-flight `Read`;
the `closed` flag makes a stray post-`Close` read a no-op; the `done` gate orders
the drain strictly after the transport. Correct by construction — the stub
transport in tests creates no concurrent read, so this rests on the reasoning,
not a test.

The size cap (`maxRequestBodyBytes`, 32 MiB) still applies: `buf` is bounded
whether filled eagerly or via the tee.

**Seam:** the swap is localized to the body-source setup in `chatCompletions`
(the per-attempt reader) — the target walk, model rewrite, and demotion are
unaffected.
