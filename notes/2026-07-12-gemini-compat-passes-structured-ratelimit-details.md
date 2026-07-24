# Gemini's OpenAI-compat 429 now passes structured RetryInfo + QuotaFailure (supersedes the 2026-07-09 prose-only finding)

**Date:** 2026-07-12
**Subject:** the OpenAI-compatibility endpoint understudy uses now serializes the
native `google.rpc` rate-limit details into the 429 body, contradicting the
prose-only observation in
[notes/2026-07-10-gemini-ratelimit-signal-shape.md](2026-07-10-gemini-ratelimit-signal-shape.md).
**Status:** empirical finding — direct captures against
`generativelanguage.googleapis.com/v1beta/openai/`.

## What changed

The 07-10 note recorded (grep-confirmed against a 2026-07-09 storm capture) that
the compat endpoint flattened `RetryInfo` to message prose with **zero** structured
`retryDelay`/`RetryInfo` fields. As of 2026-07-12 that is no longer true: the compat
429 body carries the structured `details[]` **and** the prose.

### Evidence

`gemini-2.5-flash`, per-day exhausted (`limit: 20`):

```
message prose:            "Please retry in 42.305562434s."
details[].QuotaFailure:   quotaId GenerateRequestsPerDayPerProjectPerModel-FreeTier, quotaValue "20"
details[].RetryInfo:      retryDelay "42s"
```

`gemini-2.0-flash`, free-tier `limit: 0` (429s on the first request, reporting every
applicable quota at once — including the **per-minute** ones):

```
message prose:            "Please retry in 33.32461114s."
details[].QuotaFailure:   GenerateRequestsPerDayPerProjectPerModel-FreeTier
                          GenerateRequestsPerMinutePerProjectPerModel-FreeTier   <- per-minute, structured
                          GenerateContentInputTokensPerModelPerMinute-FreeTier
details[].RetryInfo:      retryDelay "33s"
```

So structured `RetryInfo.retryDelay` and structured `QuotaFailure.quotaId` are
present for **both** per-day and per-minute quota classes — not just per-day. The
07-09 "prose is the only machine-usable signal" conclusion held for that capture but
does not hold now.

## Interpretation

The most parsimonious explanation for the ~3-day flip is that **Google changed the
compat endpoint** to pass the native `details[]` through, rather than a
per-quota-class or capture-artifact difference. The salient property for us is that
**the compat 429 body shape is not stable** — it flipped once and can flip back.

Still true from the 07-10 note: **no `Retry-After` HTTP header** ever reaches
understudy; the signal is body-only.

## Implication for the code

- Per-day handling already keys off the **structured** `quotaId` (`…PerDay…` →
  next-midnight-PT bench); that is the robust path and is correct.
- We parse and log structured `quotaId`/`quotaValue`/`retryDelay` for diagnosability.
- `RetryAfter` for non-per-day 429s is still derived from the **prose**
  (`geminiRetryDelayRE` / `withGeminiQuotaRetryAfter`), not the structured
  `retryDelay`. This is a deliberate keep, not an oversight: the two agree
  (`"33s"` vs `33.32461114s`; sub-second precision is immaterial to whole-second
  benching/forwarding), and the prose parse survived even when structured was
  absent (07-09). Because the endpoint shape is unstable, prose is the resilient
  primary and structured `retryDelay` adds no observable behavior where we already
  have prose — so deriving `RetryAfter` from the structured field is **not worth the
  churn**, and the prose path must **not** be retired.
- The `geminiRetryDelayRE` comment in `internal/understudy/providers/openai/openai.go`
  still asserts the compat 429 "carries no ... structured retryDelay field," which is
  now inaccurate; correct it (tracked as a code change, not in this note).
