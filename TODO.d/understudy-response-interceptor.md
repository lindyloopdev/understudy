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
  name what the upstream reports answered: a streamed body, and a relay that
  errors. Lindy's `Intercept` then moves its record write into the callback.
- Check what lindy's served-request record should do for a request no send
  answers, such as a cut send whose re-send is refused: the completion callback
  never fires for it, so a record written only in the callback is never written.
- Pin that the completion callback delivers the upstream-named model when an
  interceptor swaps in a body that closes through to the upstream.
- Pin that a relay that leaves the tee in place closes the wrapped upstream body.

Deferred (not now): stripping outbound `Accept-Encoding` when an interceptor is active
so responses arrive uncompressed — a transfer-size tradeoff, only if the hook fighting
gzip becomes a burden.
