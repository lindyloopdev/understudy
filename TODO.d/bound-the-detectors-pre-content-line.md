# Bound the detector's pre-content line

**Tag:** understudy / availability / streaming

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the content
boundary `confirmingReader` enforces); [[stall-before-first-content-event]]
(the pre-content window this line spans).

An upstream that opens like SSE — a `:` comment line or a field name — and then
sends no newline keeps `confirmingReader`'s line growing without bound: scan
appends every byte to `c.line` until it sees a newline or content, and nothing
caps it. A confirmed stream is safe (the line is dropped at first content); the
exposure is the pre-content line that never ends.

A legal line is not bounded — a comment, an `event:` or `id:` value, and a
`data:` line can each run to any length — but what the detector reads of one
is: whether it opens with `:` or a field name, and at the newline whether it
began with `data:`. The fix: have `scan` keep at most a line's first
`len("retry:")` bytes, the longest field name, and ignore the rest until the
newline. Clearing the line once it passes a limit would not do: a `data:` line
longer than the limit would fail the `data:` check at its newline and never be
taken for content.

Add a case to `TestShouldRelayALongStreamWithoutHoldingItInMemory` whose
upstream opens with a `:` comment and never sends a newline: what the relay
holds must stay bounded too. The test stays non-parallel — its heap readings
are process-wide.
