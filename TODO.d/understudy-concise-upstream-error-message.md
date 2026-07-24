# Build upstream error messages from parsed fields, not the raw body

**Tag:** understudy / observability / bug

**Design:** [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy).

`errorFromResponse` (providers/openai) embeds the entire raw upstream response
body in the error message whenever the body doesn't decode into
`openAIErrorEnvelope` — and Gemini's 429 is delivered as a JSON **array**
(`[{"error":{...}}]`), which the object-shaped parser rejects. So a Gemini
quota 429 logs and serves a huge truncated blob (help URLs, nested
quotaFailure, "retry in 250ms") as the `error`/message, burying the actual
signal.

The meaningful fields are already decoded into slog **attrs**
(`upstream_error_message`/`code`, `upstream_quota_id`/`value`, retry delay)
when the envelope parses — the array shape just defeats the parse, so neither
the attrs nor a tidy message is produced. Handle the array-wrapped envelope
(tolerate the shape generally), and build the error message from parsed fields
(status + message + code) — never the raw body (log it at DEBUG if it must be
retained). The attrs and the message should agree.

Cross-ref [[understudy-error-envelope-type]].
