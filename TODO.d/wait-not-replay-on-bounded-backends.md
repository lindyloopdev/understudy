# [FEATURE] Wait, not replay, on backends that bound their own admission

**Tag:** understudy / availability / admission

**Design:** [DESIGN.md §Session-Ordered
Admission](../DESIGN.md#session-ordered-admission) — *Wire verdicts*: the
comment-only disposition split and the backend property that drives it;
[DESIGN.md §Understudy](../DESIGN.md#understudy) — "Stalls: two axes, three
dispositions" (the pre-content case this carves an exception into).

Blocked on [[stall-before-first-content-event]]: the split only changes
behavior relative to the content-boundary gate it lands after. Before that
fix, comment-only already waits indefinitely — accidentally correct for a
local queueing server. After it, a backend that holds requests in its own
queue with keep-alives gets demoted and replayed at the gate timeout, which is
right for an opaque remote (the DeepSeek evidence) and wrong for a local one:
it demotes a healthy-busy backend, cancels a request that was about to be
served, and fails fast when no remote fallback is configured.

The backend marker is introduced by [[session-ordered-admission]], which does
not depend on this; reuse it here.

## Work

- **Comment-only on a marked backend waits**: within a generous budget (a
  named constant near `streamIdleTimeout`, well under a consumer's idle
  watchdog), hold the request open — no demotion, no replay, nothing sent to
  the client. Content arrives → normal pass-through. Budget exhausted → the
  existing pre-content path (demote, replay, or fail). The backend's own
  admission timeout surfacing as an error → normal error handling.
- **Pre-header silence on a marked backend is not dead.** kronk writes nothing,
  not even the `200`, while a request waits for an admission permit (see
  [[session-ordered-admission]]), so "no bytes at all → the stall path" is
  wrong there. Settle its disposition alongside the comment-only wait.
- Unmarked backends keep the eager replay unchanged.
- Tests against a fake queueing backend: comment-only within budget holds and
  then passes through intact on content; comment-only past budget takes the
  stall path; pre-header silence on an unmarked backend takes the stall path.
