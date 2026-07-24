# Fail over from a target that never responds (client-timeout / 499)

**Tag:** understudy / fallback / ha / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy).

A target whose calls produce **no upstream response** within the client's
budget — the beat times out and cancels, surfacing as a `499 Client Closed
Request` (`context canceled`, `upstream_status` None) — is not treated as
unhealthy. understudy demotes on `429` / `502` / `transientRate` /
`signalless`, but a client-side cancel is none of these, so the target stays
at the top of the list.

Observed in the 2026-07-22 `lindy review` validation: google's free-tier quota
was exhausted (backend-down), so `review-standard` failed over to
`opencode-go` (`deepseek-v4-flash` via opencode.ai/zen) — which returned zero
responses across 136 calls (not slow; non-functional in that window). Every
call client-timed-out (`499 context canceled`), so the beats never advanced to
the remaining `review-standard` targets `deepseek` / `z-ai`, and looped on
`opencode-go` until the ~15-minute beat/scene deadline killed all 20 in-flight
beats with `DeadlineExceeded`. Only the three beats served by `ollama`
(review-light, intermittent) completed.

Classify a target that **repeatedly** yields no upstream response (a streak of
client-budget timeouts/aborts, not a single benign cancel — mirror the
`transientRate` streak approach) as unavailable, so `pickTarget` advances to
the next target and the review reaches `deepseek` / `z-ai`. This is the
client-side counterpart of the latency gate / liveness deadline in
[[understudy-fallback]] (a non-responsive target *is* unavailable); it reuses
the streak substrate of [[understudy-adaptive-coordinated-backoff]].

Open: confirm `deepseek` and `z-ai` actually serve — they were never reached in
these runs, so an `opencode-go`-specific outage isn't ruled out.
