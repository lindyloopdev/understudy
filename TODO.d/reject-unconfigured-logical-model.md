# Reject an unconfigured logical model instead of substituting a catalog model

**Tag:** understudy / routing

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy);
[DESIGN.md §The `default` logical model](../DESIGN.md#understudy).

A slash-less model name is a logical model: "an operator/orchestrator-composed,
config-defined selector; the name is the whole interface" (DESIGN.md §Model
addressing). A name with no config behind it selects nothing, so it should not
resolve.

Today it does. At `understudy.go:1514` any slash-less name that missed
`backend.Models` falls into the catalog-fallback branch and is served by the
first usable backend's first advertised model. The `requestedModel !=
DefaultLogicalModel` check at 1531 gates only a WARN to the daemon's own logger
— not the fallback — so `default` and a typo'd `review` take the identical path
and differ only in whether a line lands in a log the request-originator never
reads. A misconfigured caller gets a 200 from a model it never named.

The catalog fallback is `default`'s rule alone: `default` is the reserved name
meaning "no preference", and DESIGN.md scopes the fallback to it, adding that a
`default` which resolves to nothing "is an error, not a free-trial fall".

Split the branch into the three cases the design describes:

1. **In `backend.Models`** — resolve normally. Unchanged.
2. **`default`** — catalog-fallback as today, silently. Unchanged.
3. **Anything else** — 404 naming the unconfigured model.

Case 3 carries the misconfiguration to whoever caused it, so the WARN at 1531
and any `LogRecord` substitution field both become unnecessary.

Test surface: several cases use a bare unconfigured name as incidental
scaffolding and expect a 200 — `understudy_test.go:203` ("should resolve a bare
model request via the registered provider's catalog") most explicitly. Its
assertion (catalog comes from the registered provider, not a `/v1/models` probe)
stays valid; only the model name is misplaced, and `default` proves the same
point. Check each such case is not actually asserting the fallback before
retargeting it.

Open: confirm no lindy caller addresses understudy with a bare name that is
neither configured nor `default`.
