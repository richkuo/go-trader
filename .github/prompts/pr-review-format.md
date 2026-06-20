PR review output format (this OVERRIDES any final-comment format the code-review skill specifies; keep its multi-agent review process, replace only the shape of the posted comment).

The review comment must contain nothing except the structure that follows — no preamble, summary, intro, header, emoji, or footer outside this structure. Write the entire comment as direct instructions for an agent that will read it and act on it.

Begin with a verdict line that is exactly one of: LGTM, or Needs Updates.

Sort every surviving finding into exactly one of four H3 sections. Two sections are merge-blocking: ### Needs Fixing and ### Requires Human Review. Two sections are non-blocking: ### Recommended Optional and ### Create Follow-up Issue. Every finding belongs to exactly one section.

Verdict rule: emit Needs Updates if and only if there is at least one item under ### Needs Fixing or ### Requires Human Review. Otherwise emit LGTM. A PR whose only findings are non-blocking (### Recommended Optional or ### Create Follow-up Issue) still gets LGTM — do NOT emit Needs Updates merely because comments exist.

LGTM signals the agent reading it may merge and close the PR. When the only findings are non-blocking, follow the LGTM line with the relevant ### Recommended Optional and/or ### Create Follow-up Issue sections; when there are no findings at all, LGTM stands alone with no other text.

LGTM precondition: only emit LGTM after you have inspected every changed file in the diff and checked the PR CI status. If you could not review the full diff, or could not determine CI status, you MUST NOT emit LGTM — emit Needs Updates and record the gap as an item under ### Requires Human Review.

Materiality filter (apply before writing): drop only trivia — style and naming nits, subjective preferences, micro-optimizations, and hypothetical edge cases with no realistic trigger. Anything you would prefix with 'minor' or 'nit' is trivia. Dropped trivia is not mentioned anywhere, not even as a note. Do NOT drop a substantive finding just because it is non-blocking — a real but non-merge-blocking improvement belongs under ### Recommended Optional or ### Create Follow-up Issue, never silently discarded.

Safety carve-out (overrides the materiality filter and any confidence threshold): any finding that touches money, data integrity, security, or an auto-protective mechanism (kill switch, circuit breaker, stop-loss, reconciliation, position or fill accounting) must always be surfaced, even at low confidence or small magnitude. If you cannot confirm such a finding is real, surface it under ### Requires Human Review rather than dropping it.

Within each non-empty section use a numbered list; omit any section that has no items. Each item is a single bold one-sentence title that states the item, then a newline, then a short description containing only the critical details (file:line and why it matters).

For ### Needs Fixing and ### Recommended Optional items, after the description add a newline then **Invariant:** (one sentence stating the general property the code must satisfy — what is violated, independent of the example), then a newline then **Must survive:** (1 to 3 adversarial cases beyond the example that any fix must handle: compound states, inverse scenario, boundary).

### Create Follow-up Issue is the disposition of last resort — strongly prefer keeping work in the current PR. Two conditions must BOTH hold before you suggest a new issue: (1) the finding is genuinely separate from the original issue or PR scope, AND (2) it cannot reasonably be folded into this PR — because the fix carries substantial independent scope, requires a design decision of its own, or would materially bloat or destabilize the current diff. A different file or subsystem alone does NOT make a finding a follow-up: a trivially-fixable instance of the same bug class this PR is already addressing should be fixed here (route it to ### Needs Fixing for safety-class items, otherwise ### Recommended Optional), not deferred to a new issue. When in doubt, do NOT create an issue — route the finding to another section. If the finding belongs to the current scope, it never goes here.

### Requires Human Review is reserved for findings where you genuinely cannot make a recommendation — you lack the context to judge, there is a real tradeoff only the human can resolve, or it is a safety-carve-out finding you could not confirm. Keep the description under 50 words and end it by stating precisely what the human must decide and why you cannot recommend.
