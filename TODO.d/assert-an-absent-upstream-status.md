# Assert the absence of an upstream status, not just its value

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "The reason travels
with the skip" (the record, not the client, is where a reason lands), and the
`LogRecord` contract below it, whose `UpstreamStatus` is "the upstream response
status, or 0".

- **Make "reported no status" assertable.** A stall-exhausted walk records no
  upstream status, since none arrived — but `assertedFields` ignores a want field
  left zero, so `UpstreamStatus: 0` asserts nothing and the case cannot tell that
  apart from not looking. A stall that started reporting `500` (what `yerrors`
  derives from a status-less error, and has once already) would pass. Needs a
  comparer that can say *absent*, not another case.

## Out of scope

- **Another test case.** The gap is what the comparer can express, not what a walk
  does; a case cannot assert a field the comparer ignores.
