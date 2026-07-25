# Empty logical-model target list panics `pickTarget`

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (a logical model
resolves to a priority-ordered target list),
[DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(the `api_key`/`api_key_file` key-source declaration a prune would key off).

`pickTarget` falls through to `return targets[len(targets)-1]` (`understudy.go`)
so every request has somewhere to go — which panics when the list is empty.
Nothing upstream rejects the empty case: `Config.Resolve`'s per-target
validation loop over `LogicalModelSpec.Targets` simply does not execute for an
empty slice, so a zero-target `[understudy.models.<name>]` table resolves
cleanly and panics on first use.

Reachable today only by hand-writing `targets = []`. It becomes ordinary once
`cmd/understudyd` prunes backends whose declared key source can't be resolved:
pruning a group's only backend leaves exactly this empty list.

- Guard the empty list in the library, wherever the pruning ultimately lives.
- Decide whether a zero-target logical model is a `Resolve` error or a
  request-time failure — and whether the answer differs for `default`, which
  the daemon can't serve at all if it prunes to empty.

Fix before the v0 public release.
