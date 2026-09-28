# Log request timings

**Tag:** understudy / observability

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *A bound on silence, keyed
on content*: its first value comes "from the target's own turnover" where the
consumer supplies no time limit, which needs each target's observed time to
first content.

Nothing records how long a request takes to produce its first content, so the
silence bound has no data to size its first value from, and no one can say
what the longest healthy wait on a slow target (Kronk on a large context, a
cold model load) actually is. `LogRecord` and `Attempt` record backends,
models, statuses, and errors, but no timings.

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
`id:`, `retry:` — are not content, whatever the Content-Type says. `main` has
no code that detects it. Branch `stallfix1132` does: `confirmingReader` in
`admission.go`, which session-ordered admission uses to confirm a request.
Logging built on `main` needs the same rule; when the two branches meet there
must be one detector, not two.

## Work

- **Log the time from sending a request upstream to its first content**, for
  the attempt that served it. The number the silence bound needs.
  - Tests: a request whose upstream sends content after a known delay is
    logged with that delay; framing lines before the content do not count as
    its first content.
- **Log the time from sending a request upstream to its response header.**
  The gap between this and the first-content time is the stall
  [[stall-before-first-content-event]] describes: header early, content never.
  - Tests: a request whose upstream sends its header after one delay and its
    content after another is logged with both.
- **Log the time a request was held in the admission queue before it was
  sent.** Only once session-ordered admission (branch `stallfix1132`) is on
  `main`. Shows what admission costs each request.
- (later) Timings for abandoned attempts — failovers and displacements — in
  `Attempt`.
