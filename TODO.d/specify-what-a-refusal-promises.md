# Specify what a refusal promises beyond the request that hit it

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failover walk,
the per-target health a refusal writes, and the Retry-After ladder's terminal
rung, which an all-refusing candidate list reaches.

A refused target demotes and the request replays, and what that replay costs the
caller is specified; both are now pinned by cases. What the demotion *promises* is
not written down: §Understudy says a refusal is a standing fact only an operator
clears, but not for how long understudy routes around the account, nor what
re-admits it. The health map treats it like any other immediate demotion, so a
recovery interval re-probes an account whose refusal nothing has cleared.

Also unpinned: for a walk that moves past one refusal and ends on another, nothing
asserts the record's own fields name the candidate the request answered from — its
backend, its status, the upstream's words. Only a single-target refusal is covered,
and `TestChatCompletionsFailoverRouting`'s step harness reads no field but
`Excluded`, so the case needs one that does.

Settle whether that is the intended promise — a periodic re-probe, or a bench that
only a config change or a success elsewhere lifts — and state it in §Understudy.
The behavior may already be right; what is missing is the rule a reader can hold
understudy to.
