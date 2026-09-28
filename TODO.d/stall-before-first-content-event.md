# [BUG] A target that answers 200 and then streams only keep-alives is never routed around

**Tag:** understudy / availability / streaming

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Stalls: two axes,
three dispositions" (the content boundary, pre-content stalls, and "a keep-alive
is not progress"); [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
(the probe a pre-content demotion relies on);
[DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— silence before the response header waits on every backend, so the stall gate
runs from the header to the first content, never before the header.

An upstream that sends `200` and then holds the stream open with SSE comment
lines is treated as mid-stream: no demotion, no replay — nothing gates the
attempt between the header and the first content. The stream idle watchdog
(`idleReader`, `streamIdleTimeout`) resets on any byte, so the same keep-alives
defeat it too, and the request runs until the client gives up.

Evidence, 2026-09-14, `deepseek/deepseek-v4-flash` under load:

- A direct streamed request got `200` headers after 12.5s, then only
  `: keep-alive` comment lines — no `data:` event — for 30s; a non-streamed one
  got no headers within 30s.
- Through lindy's shared daemon, ~20 concurrent chat completions from separate
  containers all ended at lindy's 10-minute idle watchdog, and others ran 15–19
  minutes, with no demotion and no failover.

The damaging shape is the one after the header. Run no gate before the header:
that wait belongs to [[session-ordered-admission]], and this fix must not
reintroduce one.

## Work

- Gate from the response header to the first content: read ahead past SSE
  comment lines (streamed) or leading JSON whitespace (non-streamed —
  `isJSONSpace`) before committing status and headers to the client. A gate
  timeout before content demotes the target and replays the request to the next
  candidate. Build the gate on `confirmingReader` (`firstcontent.go`), shared
  with [[session-ordered-admission]]'s confirmation.
- **(open) Keep-alives from a busy local server are not a stall.** Kronk sends
  its 15s keep-alive while a request with a header waits in its own queue —
  where session-ordered admission deliberately parks the probe at the default
  `QueueDepth` — and while it prefills. A plain content gate would demote and
  replay that probe behind a 6–10 minute generation. Settle a disposition with
  no per-backend flag; the candidate is progress on the request's key (a
  comment-only request waits while another request on its key, or linked set,
  is producing content, and stalls otherwise), weighed against a remote whose
  hung requests coexist with a progressing one.
- Decide what happens to comment lines and whitespace read before content: drop
  them, or forward them once content has arrived.
- Make `idleReader` count only content events, not comment lines or whitespace,
  as progress.
- Tests, each against a fake upstream:
  - `200` plus keep-alives only, nothing else on the key progressing → the
    target is demoted and the request replays to the next candidate (with no
    candidate left, it fails fast rather than hanging);
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
