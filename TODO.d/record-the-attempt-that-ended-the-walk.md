# Record the attempt that ended the walk

**Tag:** understudy / observability

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Everything a
request moved on from goes on one record, `Excluded`", while "The candidate
that *ended* a walk is carried in the record's own fields when it is what the
request answered from".

## Work

- Record the attempt that ended a walk when it is the one the request answers
  from: its status and its error go on the record alongside the name fields, so
  a failure with no alternative reads as the attempt it was rather than as a
  request no upstream answered.
- Record 0 on `Attempt.UpstreamStatus` for an abandoned attempt that never
  produced a response, rather than the 502 understudy synthesizes for a
  transport failure.
- Consumer: lindy's request log reads the record's status and error as every
  ending's, not only the ones that name a backend.
