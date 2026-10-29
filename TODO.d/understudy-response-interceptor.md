# understudy: optional response interceptor seam

**Tag:** understudy

**Design:** [DESIGN.md §Served-model provenance](../DESIGN.md#served-model-provenance)
(the seam paragraph); [DESIGN.md §Understudy](../DESIGN.md#understudy).

The enabling seam for [[understudy-provenance-reporting]] (and any future lindy-only
response rewrite). understudy stays provenance-agnostic — it offers a generic
extension point, lindy supplies the policy.

- Extend `RequestMetadata` (today `{Backend, Model, Token}`) with `RequestedModel`
  (alongside the served model) — deferred until lindy's provenance interceptor actually
  reads it, so the field lands with a consumer rather than ahead of one. (The interceptor
  owns `Content-Encoding` consistency; understudy does not transcode.)
- Move lindy's `Intercept` record write into the completion callback, holding the
  record from its usage rewrite until the callback fires.

Deferred (not now): stripping outbound `Accept-Encoding` when an interceptor is active
so responses arrive uncompressed — a transfer-size tradeoff, only if the hook fighting
gzip becomes a burden.
