# Route the unknown-logical-model fallback to the caller, not just the daemon log

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy);
[DESIGN.md §Served-model provenance](../DESIGN.md#served-model-provenance).

When a request names a logical model that isn't configured, the resolver falls
back to the default backend's first model and logs a WARN ("requested model is
not configured, using default", `understudy.go`) to the daemon's own logger
only. The config owner / request-originator — whoever set the bad logical model
— never sees it, so a misconfiguration hides in the daemon's server log.

Surface the substitution to the caller instead of (or besides) the daemon WARN:
record it on the request's `LogRecord` — a mount already reads that back on the
way out, so a consumer like lindy folds it into its own per-request entry for
free — and/or return it in the response. Also broaden coverage: the WARN fires
today only on the direct-addressing default-fallback path; the general
unknown-logical-model case should surface too.
