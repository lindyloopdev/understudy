# Admission reservation window

**Tag:** understudy / admission / scheduling

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *Reservation window
(staged)*: what the window buys (run-to-completion, fewer model swaps, fewer
canceled prefills, deterministic per-session completion) and what absence costs
(turn-granularity interleaving among running sessions plus one).

Staged after [[session-ordered-admission]]: the version without a window is
independently valuable, and the window only pays once the queue exists to hold
the next head.

## Work

- After a request completes, hold the freed admission priority for its session
  for a window sized to the consumer's inter-turn (tool-execution) gap —
  measurable from lindy transcript StepEnd timestamps; start from the observed
  gap distribution's upper-middle, not a guess.
- A turn arriving within the window is submitted directly, uncontested; a
  lapse releases the next queue head.
- An explicit session-end signal from the consumer (lindy emits one per beat)
  releases the reservation immediately, so the timer covers only abnormal
  termination and non-participating consumers. It is the same signal
  [[session-ordered-admission]] uses to forget a session's age.
- A session in a long silent tool run correctly loses the reservation — the
  window covers short gaps, not max-gap; holding capacity for a non-requesting
  session is the waste the discipline exists to prevent. It keeps its rank.
- Tests: a session returning within the window keeps its place with no
  cancel/resubmit churn; a lapse releases the head; explicit release bypasses
  the timer; a session with no queue behind it keeps running regardless.
