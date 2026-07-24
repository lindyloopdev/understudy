# understudy: optional response interceptor seam

**Tag:** understudy

**Design:** [UNDERSTUDY.md §Served-model provenance](../UNDERSTUDY.md#served-model-provenance)
(the seam paragraph); [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy).

The enabling seam for [[understudy-provenance-reporting]] (and any future lindy-only
response rewrite). understudy stays provenance-agnostic — it offers a generic
extension point, lindy supplies the policy.

The injected `ResponseInterceptor` (functional option on `New`), relay-point
invocation with in-place `resp` mutation, `nil`-passthrough, `RequestMetadata{Backend,
Model, Token}`, stale-`Content-Length` dropping, upstream-strip-then-interceptor ordering
(interceptor-set headers survive), interceptor-error surfacing, and original-body close
on swap are built and covered. Remaining:

- Extend `RequestMetadata` (today `{Backend, Model, Token}`) with `RequestedModel`
  (alongside the served model) — deferred until lindy's provenance interceptor actually
  reads it, so the field lands with a consumer rather than ahead of one. (The interceptor
  owns `Content-Encoding` consistency; understudy does not transcode.)

Deferred (not now): stripping outbound `Accept-Encoding` when an interceptor is active
so responses arrive uncompressed — a transfer-size tradeoff, only if the hook fighting
gzip becomes a burden.
