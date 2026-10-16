# Serve a 400'd conversation from the next target

**Tag:** understudy / availability / failover

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A target that
cannot serve *this* conversation is routed around, not adapted": how a history
rejection is recognized.

## Work

- **Serve from the next target when one answers a `400` to a conversation
  another target served last, whatever the message says.** It adds to matching
  a provider's exact wording (`TestChatCompletionsFailoverRouting`, "rejects the
  request's history"), not replaces it.
