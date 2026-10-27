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
- Finish the completion callback that delivers the model the response names (see
  [[record-the-model-the-response-names]]), so lindy's served-request record can
  name what the upstream reports answered: a displaced and re-sent send, a
  streamed body, and a relay that errors. Lindy's `Intercept` then moves its
  record write into the callback.
- Pin that the completion callback delivers the upstream-named model when an
  interceptor reads only the part of the upstream body that names it before swapping it, or swaps in
  a body that closes through to the upstream, and that it fires once with `""` when
  the upstream response names no model, and that an interceptor which swaps the body
  without reading the upstream's gets `""` there and in the log record.
- Guard the completion callback against a panic: understudy does not trust its
  caller to, and a panic unwinding out of `relay` strands the admission slot.
  Log it at ERROR.
- Pin that a relay that leaves the tee in place closes the wrapped upstream body.

Deferred (not now): stripping outbound `Accept-Encoding` when an interceptor is active
so responses arrive uncompressed — a transfer-size tradeoff, only if the hook fighting
gzip becomes a burden.
