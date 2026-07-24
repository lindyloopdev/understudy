# Proactive per-upstream rate pacer set aside for retry-after-aware demotion recovery

**Date:** 2026-07-11
**Subject:** why the planned proactive RPM/TPM token-bucket pacer (was
`TODO.d/understudy-request-rate-limiter.md`) was dropped in favour of
retry-after-timed demotion recovery
**Status:** rejection reasoning — the chosen mechanism ships in
`internal/understudy/understudy.go` (`recordRateLimited` + `pickTarget`'s
`readmitAt` path). Reconsider the pacer only under the triggers below.

## What the pacer would have done

A per-upstream-account token bucket (requests/min, tokens/min) pacing outbound
requests in `chatCompletions` and queuing the rest, so understudy stays *under* a
provider's rate budget instead of overshooting and 429-storming — self-seeding its
RPM from a Gemini `QuotaFailure` limit. Orthogonal to the concurrency cap
([[understudy-concurrency-limiter]]).

## Why it was dropped

Working through the actual Gemini setup (see
[[project_understudy_no_free_paid_metadata]]) inverted the case for pacing:

- **The scarce quota is daily (RPD ~50), and tiny.** A real dev session spends it
  in a couple of hours regardless, so there is nothing to *conserve* by pacing —
  stretching 50 requests out is pointless.
- **The RPM throttle is a minor, transient inconvenience.** Its retry-after is
  seconds-to-~1min (observed ~50s). Waiting it out (or falling back to the paid
  target during it) is acceptable; it only bites during the first <50 requests of
  the day.
- **Only one free provider + no free/paid metadata**, so "failover" spends money
  — but the user is fine paying the fallback during a >30s throttle *provided
  Gemini re-enters rotation the instant its limit clears*.

That requirement is exactly **retry-after-aware demotion recovery**: bench the
rate-limited target for its advertised retry-after (fall back to the paid target
for the gap), then re-admit it as a half-open probe when the limit clears. It
reuses the existing demote/failover substrate and the retry-after we already
extract — no new pacing subsystem, and it delivers the actual goal (use Gemini
free whenever it's available, pay only while it's throttled).

Pacing would have *prevented* the 429s; recovery-timing makes the 429s cheap and
self-correcting, which is what the situation actually wanted.

## When to reconsider the pacer

- A provider with **no fallback** (pacing is the only lever to stay usable).
- 429s that are **costly** — a provider that bans or charges on rejection, where
  emitting them at all is bad, not just slow.
- **TPM smoothing** — a token-per-minute budget that a few token-heavy requests
  blow, which request-count recovery can't see.
- A **pool of free providers** worth pacing across (blocked today on that pool
  existing and on free/paid metadata — [[project_understudy_no_free_paid_metadata]]).
