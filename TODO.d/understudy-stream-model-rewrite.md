# Pool the model-rewrite prefix buffer

**Tag:** understudy / quick

**Design:** [DESIGN.md §understudy](../DESIGN.md#understudy).

`rewriteModel` (in `internal/understudy`) scans a chat request body to the
top-level `model`, rewrites that value via an injected transform, and streams
the rest. To locate `model` it buffers the body up to that field (the prefix).

Buffering the pre-`model` prefix is intrinsic, not incidental: routing must
know the backend (parsed from `model`) *before* it can dial the upstream to
forward to, so the scan up to `model` must complete — and hold its bytes to
replay — before any byte leaves. A lazy `io.Pipe` can't route before
forwarding without re-tangling routing into the rewriter.

## Prefix size is observed, not hard-capped

A hard cap that *rejects* an over-size prefix was considered and rejected.
Understudy's clients are Lindy-controlled and emit `model` early, so the
rewrite succeeds even when `model` is late; a hard reject would therefore only
ever fail a serviceable request — a latent cliff certain to surface years later
in the wild against a request we could have served. Instead `chatCompletions`
logs an ERROR when the prefix scan exceeds `maxPrefixScan` (~64KiB) and proceeds
normally, so an unexpectedly large prefix is visible without breaking the
request.

The *whole* body, by contrast, is hard-capped. Within-request failover buffers
the entire body to replay it to the next target, so an unbounded body is a
memory-exhaustion vector the prefix scan doesn't cover. `chatCompletions` wraps
the read in `http.MaxBytesReader` at `maxRequestBodyBytes` (32 MiB) and rejects
an over-cap body with 413. The cap sits well above any legitimate body — image
bytes dwarf token counts, but 32 MiB clears any realistic multimodal request —
so it guards only memory-exhausting abusive payloads, never the serviceable
request the prefix rule protects. The 413 logs at ERROR (via `levelForStatus`),
the operator tripwire.

Revisit either bound only if its tripwire ever fires in practice.

## Pool the prefix buffer

Pool the prefix buffer (`sync.Pool`) to cut per-request allocation churn under
concurrency. `Put` must guard on capacity so an outlier buffer doesn't bloat
the pool. Defer until a benchmark shows allocation pressure; pooling on spec is
premature.

## Future

- `encoding/json/v2` streaming, if/when stable, may simplify the mechanism.
