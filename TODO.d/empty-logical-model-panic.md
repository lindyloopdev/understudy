# Empty logical-model target list panics `pickTarget`

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (a logical model
resolves to a priority-ordered target list),
[DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(the `auth = "auto"` drop that constructs empty lists internally).

`pickTarget` falls through to `return targets[len(targets)-1]` (`understudy.go`)
so every request has somewhere to go — which panics when the list is empty.

`Resolve` rejects a zero-target model, so the config-file route is closed, but the
panic stays reachable two ways: `BackendConfig` and `LogicalModel` are exported, so
an embedder assembling `Models` by hand bypasses that check; and the `auth = "auto"`
drop ([[auth-requirement-and-key-env-source]]) builds pruned target lists
*downstream* of it.

- Guard the empty list in `pickTarget`. Observable at the request boundary — a
  request against a hand-built empty model returns an error rather than crashing
  the process.
- Decide whether `default` pruning to empty differs from any other model doing so.
  The daemon cannot serve at all in that case, so it may warrant its own failure
  rather than the generic one; settle alongside the drop, which is where the case
  arises.

Fix before the v0 public release, and before the drop lands.
