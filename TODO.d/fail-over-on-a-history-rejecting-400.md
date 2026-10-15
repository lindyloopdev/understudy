# Consider serving a 400'd conversation from the next target

**Tag:** understudy / availability / failover

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A target that
cannot serve *this* conversation is routed around, not adapted."

Consider whether the walk should serve from the next target when one answers a
`400` to a conversation another target served last, whatever the error message
says. Catching a provider's rejection of a history before its wording has been
seen — and keeping that catch if the wording changes — is weighed against a
malformed request being tried at more targets before its `400` reaches the
client. It would add to matching a provider's exact wording
(`TestChatCompletionsFailoverRouting`, "rejects the request's history"), not
replace it: the rule needs a record of the target that served the conversation
last, and that record lapses once the conversation sits idle.
