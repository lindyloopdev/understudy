# Credential requirement vocabulary (`auth`)

**Tag:** understudy / config

**Design:** [DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(credential sourcing, the `auth` vocabulary and its availability framing),
[DESIGN.md §Understudy](../DESIGN.md#understudy) (the availability layer that
carries a credential-less backend).

Build the `auth` declaration: what kind of fact an absent credential is.

- Add `auth` (`required` default / `none` / `auto`, plus `optional` reserved).
- Express the combination rules as tags, not a method: `excluded_if`/`required_if`
  against the `auth` value cover "a key source under `none`" and "no source under
  `required`/`auto`". The exactly-one-of rule is likewise `excluded_with` naming
  the sibling sources — `excluded_with` takes a list and fires if any named field
  is set, so it stays one tag per field rather than growing pairwise.
- Reject `auth = "optional"` with a reserved-value error, following the
  `thinking=true` precedent in `target.go`, and carry the matching inline
  `TODO(TODO.d/…)` marker.
- Under `auth = "auto"`, an empty key must **not** fail validation. The backend
  stays in the configuration and is instead **unavailable** — seed the health
  state the failover walk already consults, so `pickTarget` passes over it exactly
  as it does a demoted target. Nothing is pruned: no backend removed, no target
  rewritten, no model emptied.
- Under `auth = "required"`, an empty key is a validation error naming both the
  backend and the source that should have supplied it.
- Under `auth = "none"`, exclude the backend from credential-refusal failover: a
  `401` from it reaches the client instead of demoting the target and replaying
  onto a sibling. `isCredentialRefused` currently fires on every `401`, which is
  unobservable only while no config can declare `auth = "none"` — landing the
  vocabulary makes the divergence real, so the two must ship together. Needs the
  `auth` value at the failover decision, which `Backend` does not carry today.

Depends on [[resolve-validate-split]] for the stage that runs these rules; the
`api_key_env` source itself is built.

Any tag change here is a contract change lindy sees; relaxing a constraint keeps
existing configs valid, so the two sides need not land together.

Deferred: three key-source fields work today, but a fourth would be the point to
reconsider a `[backends.<name>.key]` sub-table with a source discriminator rather
than a fourth sibling field.
