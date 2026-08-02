# Give a relayed refusal an envelope type that is not server_error

**Tag:** understudy / api

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — refused access and
the walk that relays it once every candidate has refused; the error envelope whose
`type` a client classifies on rather than the status.

`errorType` falls back to `errTypeServer` for any relayed upstream failure that
carries no type of its own, so a refusal reaching the client after the walk is
spent arrives as `403` typed `server_error`. For a relayed 502 that fallback is
right; for a refusal it tells the client the upstream broke when it did not, and
lindy classifies on the envelope `type`, not the status.

- Settle which type it should carry. `errTypeAuth` is understudy's own — the
  handler uses it when *its* session token fails — so reusing it would have a
  client rotate a key that is fine. `errTypeUpstreamUnavailable` is closer but
  claims absence rather than refusal. A new type is the third option, and costs a
  consumer-visible vocabulary change.
- Reaching this at all needs the exhausted-refusal case
  [[specify-what-a-refusal-promises]], which is what observes the relayed status
  and type today.
