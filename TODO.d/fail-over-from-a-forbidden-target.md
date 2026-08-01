# [BUG] A 403 neither demotes its target nor fails over

**Tag:** understudy / fallback / ha

Blocks lindy's cost-reduction plan: until this lands, lindy's reviews fall
through to metered DeepSeek instead of the already-paid GLM plan — see
[lindy TODO.d/cost-reduction.md](https://gitlab.com/flimzy/lindy/-/blob/main/TODO.d/cost-reduction.md).

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the failover walk
and the per-target health it reads; the Retry-After ladder's "next target
healthy → fail over in place" rung.

`isCredentialRefused` matches 401 and 402 only, so a 403 satisfies neither arm
of the demote branch (`demote || isCredentialRefused(err)`) nor of the
within-request replay condition
(`sig.condition == sustainedRate || isCredentialRefused(err)`). A target that
answers 403 is therefore left healthy, is not walked past, and its refusal is
surfaced to the caller while untried healthy targets in the same logical model
are never attempted.

## Observed

A logical model with four targets, whose second answers 403:

```
targets = ["google/gemini-2.5-flash", "opencode-go/deepseek-v4-flash",
           "deepseek/deepseek-v4-flash", "z-ai/glm-4.7?thinking=false"]
```

`opencode-go` refuses that model outright — the account has not opted in to its
China-hosted build:

```
HTTP 403 {"type":"error","error":{"type":"RegionError","message":"The latest
version of this model is only available hosted in China and requires explicit
opt in: …"}}
```

Every request died there. Fifteen of eighteen concurrent consumers failed with
`rpc error: code = Unavailable desc = opencode session error: APIError:
Forbidden`, none of them reaching a working target — while direct probes taken
minutes later showed the two targets *after* it both serving:

| target | probe |
| --- | --- |
| `opencode-go/deepseek-v4-flash` | **HTTP 403** RegionError |
| `deepseek/deepseek-v4-flash` | HTTP 200 |
| `z-ai/glm-4.7` | HTTP 200 |

The refusal is permanent and target-scoped: it depends on the account's opt-in
for that backend, not on the request, so every retry against that target refuses
identically.

## Solution

Treat a 403 as the target refusing this caller — the same family as 401
(credential rejected) and 402 (out of funds), differing only in *why* the
account may not use the resource. Include it in the classifier so the target
demotes and the request replays onto the next untried candidate. The classifier
then covers refusal rather than credentials specifically, so `isCredentialRefused`
wants a name that says so.

One case to decide at implementation time: a 403 that is *request*-scoped rather
than target-scoped — a content-policy refusal, say — would replay onto every
candidate and be refused by each. That ends in the same terminal error as today
after spending the list, so the failure mode is cost rather than correctness, but
it is worth knowing which 403s a provider actually sends before assuming the
target-scoped reading.
