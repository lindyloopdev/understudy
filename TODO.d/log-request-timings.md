# Log request timings

**Tag:** understudy / observability

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *A bound on silence, keyed
on content*: its first value comes "from the target's own turnover" where the
consumer supplies no time limit, which needs each target's observed time to
first content.

The silence bound still has no data until lindy records the field next to what
only it knows, and no one can say what the longest healthy wait on a slow
target (Kronk on a large context, a cold model load) actually is.

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
`id:`, `retry:` — are not content, whatever the Content-Type says. When
session-ordered admission lands on `main`, use `confirmingReader`
(`firstcontent.go`) as its detector rather than building a second one.

## Work

- **Log the time a request was held in the admission queue before it was
  sent.** Only once session-ordered admission (branch `stallfix1132`) is on
  `main`. Shows what admission costs each request.
- (later) Timings for abandoned attempts — failovers and displacements — in
  `Attempt`.
