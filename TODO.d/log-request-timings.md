# Log request timings

**Tag:** understudy / observability

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *A first-content timeout*:
its first value comes from the target's own turnover, which needs each target's
observed time to first content.

lindy's request log records both timings, and its
notes/2026-09-30-time-to-first-content-measurements.md sized the timeout's
first value from them. What remains is the timings this does not yet record.

## Where it belongs

- **Understudy measures.** Only it knows when it sent the request upstream and
  when that attempt's first content passed through its relay. An embedder
  outside the handler sees queueing, failovers, and retries folded together.
  The timings go on the per-request record the embedder reads (`LogRecord`,
  via `WithLogCtx` / `LogRecordFromContext`).
- **lindy records.** Its request log writes the new fields next to what only
  it knows (run, beat, session). Analysis across runs happens there. A
  separate change in the lindy repo, after this one lands.

## First content has one definition

"First content" means: a stream's first `data:` event; any other body's first
non-whitespace byte. Framing lines — SSE comments (keep-alives), `event:`,
`id:`, `retry:` — are not content, whatever the Content-Type says.
`confirmingReader` (`firstcontent.go`) is the one detector: the relay's single
callback both logs the time to first content and confirms the request for
session-ordered admission.

## Work

- (later) Timings for abandoned attempts — failovers and displacements — in
  `Attempt`.
