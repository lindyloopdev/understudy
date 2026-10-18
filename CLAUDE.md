# Understudy

A host-side LLM proxy: key custody, logical models, rate-limit
classification, and availability execution. lindy is a consumer.

## Doc conventions

- **DESIGN.md is normative, not descriptive.** It specifies the intended
  design; it never reports how much of it is built. Code is ground truth for
  what exists; DESIGN.md is ground truth for what is intended. A divergence
  between them is tracked work, not a doc defect — so don't "correct"
  DESIGN.md to match today's code. File (or update) the `TODO.d/` item, and
  link it from the section whose design is outstanding.
- **`TODO.d/` = thin work items**, each with a
  `**Design:** [DESIGN.md §…](DESIGN.md#anchor)` backref. No decisions live
  here.
- **`notes/` = rejection reasoning + empirical findings.** Date-prefixed for
  point-in-time investigations; plain name for living reference. A note's
  content is a historical record and is not changed: a correction goes in a
  dated addendum. Errata — links, typos, vocabulary — are fixed in place.
- **DESIGN.md headings carry explicit `<a id="slug">` anchors.** Backrefs point
  at the anchor, not the heading text, so they survive rewording.
