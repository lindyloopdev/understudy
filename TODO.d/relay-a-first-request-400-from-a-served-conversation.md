# Relay a first-request 400 from a served conversation

**Tag:** understudy / availability / failover

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A target that
cannot serve *this* conversation is routed around, not adapted": how a history
rejection is recognized.

## Work

- **Relay a `400` to the client when it answers the first request of a
  conversation another target served last.** A request carrying no history has
  nothing for the target to reject, so the failover that reads the affinity
  record does not reach it; decide whether the record alone should.
