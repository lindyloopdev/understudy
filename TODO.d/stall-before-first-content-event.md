# [BUG] A target that answers 200 and then streams only keep-alives is never routed around

**Tag:** understudy / availability / streaming

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Stalls: two axes,
three dispositions" (the content boundary, pre-content stalls, and "a keep-alive
is not progress"); [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
(the probe a pre-content demotion relies on);
[DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— *A bound on silence, keyed on content*, which now runs a gate before the
header too (retrying the same target, preserving rank); this file's gate is
the separate question of demoting a target and replaying to a *different*
candidate, and the two now need to agree on precedence — see the open note in
[[session-ordered-admission]]'s own bound-on-silence item.

An upstream that sends `200` and then holds the stream open with SSE comment
lines is pre-content: understudy withholds its status, headers, and comment
lines until the first content, so nothing is committed to the client and the
attempt could still be replayed. Nothing acts on that yet: no gate times the
wait between the header and the first content, and the stream idle watchdog
(`idleReader`, `streamIdleTimeout`) resets on any byte, so the same
keep-alives defeat it too, and the request still runs until the client gives
up.

Evidence, 2026-09-14, `deepseek/deepseek-v4-flash` under load:

- A direct streamed request got `200` headers after 12.5s, then only
  `: keep-alive` comment lines — no `data:` event — for 30s; a non-streamed one
  got no headers within 30s.
- Through lindy's shared daemon, ~20 concurrent chat completions from separate
  containers all ended at lindy's 10-minute idle watchdog, and others ran 15–19
  minutes, with no demotion and no failover.

The damaging shape here is the one after the header — this file's demote-and-
replay disposition never applied before one arrived, and still does not.
Silence before the header is no longer ungated, though: [[session-ordered-admission]]'s
own bound on silence now covers it, by retrying the same target rather than
replaying to a different one. The two gates answer different questions
(replay elsewhere vs. retry in place) for what can be the same triggering
silence, and which takes precedence is unsettled.

## Work

- **Test a body that is only leading whitespace before it ends.** The relay
  withholds an upstream's status, headers, and pre-content bytes until its
  first content, and releases them when the body ends if no content ever
  came; a body of only keep-alive lines is tested, one of only leading
  whitespace is not.
  - Tests: an upstream that sends its status, headers, and only leading
    whitespace before its body ends reaches the client with that status,
    those headers, and those bytes.
- Gate from the response header to the first content: a gate timeout before
  content demotes the target and replays the request to the next candidate.
  The read-ahead it needs is built — the relay withholds status, headers, and
  pre-content bytes until `confirmingReader` (`firstcontent.go`) finds
  content — so a timed-out attempt has committed nothing to the client.
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
