# [BUG] A target that answers 200 and then streams only keep-alives is never routed around

**Tag:** understudy / availability / streaming

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Stalls: two axes,
three dispositions" (the content boundary, pre-content stalls, and "a keep-alive
is not progress"); [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
(the probe a pre-content demotion relies on);
[DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— a comment-only request on a backend that bounds its own admission is
queued-and-alive (wait), not stalled; the eager replay below applies to opaque
remotes. The wait-side carve-out lands in
[[wait-not-replay-on-bounded-backends]], sequenced immediately after this fix —
the split only has meaning relative to it.

The stall gate (`callWithHeaderGate`) returns as soon as the first response header
arrives, so an upstream that sends `200` and then holds the stream open with SSE
comment lines is treated as mid-stream: no demotion, no replay. The stream idle
watchdog (`idleReader`, `streamIdleTimeout`) resets on any byte, so the same
keep-alives defeat it too, and the request runs until the client gives up.

Evidence, 2026-09-14, `deepseek/deepseek-v4-flash` under load:

- A direct streamed request got `200` headers after 12.5s, then only
  `: keep-alive` comment lines — no `data:` event — for 30s; a non-streamed one
  got no headers within 30s (which the existing gate already catches).
- Through lindy's shared daemon, ~20 concurrent chat completions from separate
  containers all ended at lindy's 10-minute idle watchdog, and others ran 15–19
  minutes, with no demotion and no failover.

## Work

- Extend the stall gate from the first header to the first content: read ahead
  past SSE comment lines (streamed) or leading JSON whitespace (non-streamed —
  `isJSONSpace`) before committing status and headers to the client. A gate
  timeout before content takes the existing pre-header path (`errHeaderStall`,
  `recordStalled`, replay to the next candidate). Rename the error and gate to
  match the content boundary.
- Decide what happens to comment lines and whitespace read before content: drop
  them, or forward them once content has arrived.
- Make `idleReader` count only content events, not comment lines or whitespace,
  as progress.
- Leave backends marked as bounding their own admission alone: if
  [[session-ordered-admission]] has landed, its marker and parked-probe gate
  exemption must survive this change — comment-only and pre-header silence on
  a marked backend keep waiting.
- Tests, each against a fake upstream:
  - `200` plus keep-alives only → the target is demoted and the request replays
    to the next candidate (with no candidate left, it fails fast rather than
    hanging);
  - keep-alives, then `data:` events → the response passes through intact and
    the target is not demoted;
  - non-streamed leading whitespace, then JSON → passes through intact;
  - content, then keep-alives only → the idle deadline cancels the request
    without demoting the target.
- Recovery: a probe against a target demoted for a pre-content stall pays the
  full gate before it learns anything. Settle how that composes with
  [[demand-triggered-recovery-probe]] rather than probing it like a fast
  connection failure. Gate and backoff constants stay with
  [[understudy-adaptive-coordinated-backoff]].
