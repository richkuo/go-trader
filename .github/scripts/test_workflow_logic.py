"""Regression tests pinning the security-critical routing shell in claude.yml.

The push-access boundary of the @claude workflow lives in two hand-written
shell steps inside .github/workflows/claude.yml:

  - verify_invocation — decides whether a comment is a real @claude invocation,
    and (for claude[bot]'s own comments) whether it is the exact one-line
    "@claude review" self-trigger vs. review output that merely quotes @claude.
  - classify_mode — routes an invocation to the read-only `review` job or the
    push-capable `implement` job.

This repo is PUBLIC, so an author-association misjudgment here would let an
external contributor's comment reach a push-capable run; a later edit could
silently widen the write path. This test pins every documented outcome. Like
test_patch_claude_comment.py it does NOT re-implement the logic (a copy would
drift and never catch a workflow edit) — it EXTRACTS the real `run:` block out
of the workflow YAML and executes it in a bash subprocess with injected env,
so a change to the workflow is what the test runs against.

Run under pytest (this repo's test runner):
  uv run pytest .github/scripts/test_workflow_logic.py
"""

import os
import re
import subprocess
import tempfile

HERE = os.path.dirname(__file__)
CLAUDE_YML = os.path.abspath(os.path.join(HERE, "..", "workflows", "claude.yml"))

VERIFY_STEP = "Verify @claude is an actual invocation (not in a code block or example)"
CLASSIFY_MODE_STEP = "Classify invocation route (review, implement, or fix-pr)"

# A PR issue_comment carries a non-empty pull_request.url; an issue comment does not.
PR_URL = "https://api.github.com/repos/o/r/pulls/5"


def _read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def extract_step_run_block(yml_text, step_name):
    """Return the dedented body of a step's `run: |` block, verbatim from the YAML."""
    lines = yml_text.split("\n")
    name_pat = re.compile(r"^(\s*)- name:\s*" + re.escape(step_name) + r"\s*$")
    start = None
    step_indent = None
    for idx, ln in enumerate(lines):
        m = name_pat.match(ln)
        if m:
            start = idx
            step_indent = len(m.group(1))
            break
    if start is None:
        raise AssertionError(
            f"step '{step_name}' not found in workflow — renamed? Update this extractor."
        )

    run_pat = re.compile(r"^(\s*)run:\s*\|\s*$")
    next_step_pat = re.compile(r"^ {%d}- name:" % step_indent)
    run_idx = None
    run_indent = None
    for idx in range(start + 1, len(lines)):
        if next_step_pat.match(lines[idx]):
            break
        m = run_pat.match(lines[idx])
        if m:
            run_idx = idx
            run_indent = len(m.group(1))
            break
    if run_idx is None:
        raise AssertionError(
            f"no `run: |` block found under step '{step_name}' — structure changed?"
        )

    body = []
    for idx in range(run_idx + 1, len(lines)):
        ln = lines[idx]
        if ln.strip() == "":
            body.append("")
            continue
        cur_indent = len(ln) - len(ln.lstrip())
        if cur_indent <= run_indent:
            break
        body.append(ln)

    non_blank = [l for l in body if l.strip() != ""]
    if not non_blank:
        raise AssertionError(f"step '{step_name}' has an empty run block")
    min_indent = min(len(l) - len(l.lstrip()) for l in non_blank)
    return "\n".join(l[min_indent:] if l.strip() != "" else "" for l in body)


def _run_block(script, env_overrides, output_key):
    """Execute an extracted run block with injected env; return the last value it
    wrote to GITHUB_OUTPUT under output_key (the real value is written after any
    attacker-controlled heredoc body, so last-wins is the authoritative one)."""
    with tempfile.TemporaryDirectory() as d:
        out_path = os.path.join(d, "github_output")
        open(out_path, "w").close()
        env = dict(os.environ)
        env.update(env_overrides)
        env["GITHUB_OUTPUT"] = out_path
        r = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True)
        value = None
        prefix = output_key + "="
        with open(out_path, encoding="utf-8") as f:
            for line in f:
                if line.startswith(prefix):
                    value = line[len(prefix):].rstrip("\n")
        if value is None:
            raise AssertionError(
                f"run block wrote no {output_key}= line to GITHUB_OUTPUT; stderr:\n{r.stderr}"
            )
        return value


def run_classify_mode(event_name, stripped, pr_url="", pr_author_assoc="", pr_author_login="", flow=""):
    script = extract_step_run_block(_read(CLAUDE_YML), CLASSIFY_MODE_STEP)
    return _run_block(
        script,
        {
            "EVENT_NAME": event_name,
            "PR_URL": pr_url,
            "FLOW": flow,
            "STRIPPED": stripped,
            "PR_AUTHOR_ASSOC": pr_author_assoc,
            "PR_AUTHOR_LOGIN": pr_author_login,
        },
        "mode",
    )


def run_verify_invocation(event_name, body, trigger_actor="someuser"):
    script = extract_step_run_block(_read(CLAUDE_YML), VERIFY_STEP)
    return _run_block(
        script,
        {
            "EVENT_NAME": event_name,
            "COMMENT_BODY": body,
            "REVIEW_BODY": body,
            "ISSUE_BODY": body,
            "TRIGGER_ACTOR": trigger_actor,
        },
        "invoked",
    )


# --- classify_mode routing (review vs. push-capable implement/fix-pr) ---

def test_trusted_member_pr_comment_no_review_word_is_fix_pr():
    # Consolidated routing: any non-"review" @claude comment on a
    # trusted-author PR runs the review-reconciliation playbook in place.
    assert run_classify_mode(
        "issue_comment", "@claude correct the lint error", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "fix-pr"


def test_trusted_owner_pr_comment_is_fix_pr():
    assert run_classify_mode(
        "issue_comment", "@claude address the feedback", pr_url=PR_URL, pr_author_assoc="OWNER"
    ) == "fix-pr"


def test_claude_bot_authored_pr_is_fix_pr():
    # work-on-issue PRs are authored by claude[bot] (association NONE) — the login
    # check, not the association, must admit them.
    assert run_classify_mode(
        "issue_comment", "@claude address the feedback", pr_url=PR_URL,
        pr_author_assoc="NONE", pr_author_login="claude[bot]"
    ) == "fix-pr"


def test_external_author_pr_comment_is_review_only():
    # PUBLIC repo: an external/fork-authored PR (association NONE) never earns
    # push, even from a trusted commenter (the job trigger already gated it).
    assert run_classify_mode(
        "issue_comment", "@claude fix the lint error", pr_url=PR_URL, pr_author_assoc="NONE"
    ) == "review"


def test_contributor_author_pr_comment_is_review_only():
    assert run_classify_mode(
        "issue_comment", "@claude fix this", pr_url=PR_URL, pr_author_assoc="CONTRIBUTOR"
    ) == "review"


def test_review_keyword_forces_review_even_for_trusted_author():
    assert run_classify_mode(
        "issue_comment", "@claude review this carefully", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "review"


def test_review_keyword_after_model_shorthand_is_review():
    assert run_classify_mode(
        "issue_comment", "@claude sonnet review", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "review"


def test_review_and_fix_loses_push_on_purpose():
    assert run_classify_mode(
        "issue_comment", "@claude review and fix it", pr_url=PR_URL, pr_author_assoc="OWNER"
    ) == "review"


def test_review_word_later_in_sentence_no_longer_forces_review():
    # Keyword routing: only the FIRST word after @claude counts, so an
    # instruction that merely mentions review keeps the push-capable
    # review-fixing route.
    assert run_classify_mode(
        "issue_comment", "@claude fix the review comments", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "fix-pr"


def test_fix_keyword_routes_to_fix_pr():
    # "fix" is no longer a special keyword — it lands on fix-pr like any
    # other non-review trusted-PR comment.
    assert run_classify_mode(
        "issue_comment", "@claude fix", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "fix-pr"


def test_fix_keyword_after_model_shorthand_routes_to_fix_pr():
    assert run_classify_mode(
        "issue_comment", "@claude opus fix and be thorough", pr_url=PR_URL, pr_author_assoc="OWNER"
    ) == "fix-pr"


def test_old_fix_pr_spelling_routes_to_fix_pr():
    # The old trigger spelling routes like any non-review ask now.
    assert run_classify_mode(
        "issue_comment", "@claude fix-pr", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "fix-pr"


def test_fix_keyword_untrusted_pr_author_is_review_only():
    # Must survive: the fix keyword never earns push over an
    # external-author PR — fail-closed to read-only review (PUBLIC repo).
    assert run_classify_mode(
        "issue_comment", "@claude fix", pr_url=PR_URL, pr_author_assoc="NONE"
    ) == "review"


def test_fix_keyword_on_plain_issue_is_implement():
    # No PR context: the issue path wins before keyword routing.
    assert run_classify_mode(
        "issue_comment", "@claude fix", pr_url="", pr_author_assoc="MEMBER"
    ) == "implement"


def test_fix_keyword_on_inline_review_surface_stays_review():
    # PR-review surfaces are always read-only regardless of keyword.
    assert run_classify_mode(
        "pull_request_review_comment", "@claude fix", pr_url=PR_URL, pr_author_assoc="OWNER"
    ) == "review"


def test_pull_request_review_surface_is_review():
    assert run_classify_mode(
        "pull_request_review", "@claude fix this", pr_url=PR_URL, pr_author_assoc="MEMBER"
    ) == "review"


def test_pull_request_review_comment_surface_is_review():
    assert run_classify_mode(
        "pull_request_review_comment", "@claude fix this", pr_url=PR_URL, pr_author_assoc="OWNER"
    ) == "review"


def test_issues_event_is_implement():
    assert run_classify_mode(
        "issues", "@claude build this feature", pr_author_assoc="MEMBER"
    ) == "implement"


def test_docs_release_flow_routes_to_implement_even_on_pr():
    # A resolved docs/release FLOW wins before PR routing.
    assert run_classify_mode(
        "issue_comment", "@claude sync-docs", pr_url=PR_URL,
        pr_author_assoc="MEMBER", flow="sync-docs"
    ) == "implement"


def test_issue_comment_on_issue_is_implement():
    # No PR_URL: an issue_comment on a plain issue is the issue-workflow path.
    assert run_classify_mode(
        "issue_comment", "@claude implement this", pr_url="", pr_author_assoc="MEMBER"
    ) == "implement"


# --- verify_invocation claude[bot] self-trigger guard ---

def test_exact_one_line_self_trigger_fires():
    assert run_verify_invocation("issue_comment", "@claude review", "claude[bot]") == "true"


def test_leading_blank_line_still_fires():
    # The regression this fix targets: a leading blank line used to leave a
    # newline that failed the one-line count, silently suppressing self-review.
    assert run_verify_invocation("issue_comment", "\n@claude review", "claude[bot]") == "true"


def test_leading_blank_and_indentation_still_fires():
    assert run_verify_invocation("issue_comment", "  \n   @claude review  ", "claude[bot]") == "true"


def test_trailing_carriage_return_still_fires():
    assert run_verify_invocation("issue_comment", "@claude review\r", "claude[bot]") == "true"


def test_model_shorthand_self_trigger_fires():
    assert run_verify_invocation("issue_comment", "@claude opus review", "claude[bot]") == "true"


def test_effort_token_self_trigger_fires():
    assert run_verify_invocation("issue_comment", "@claude review effort:high", "claude[bot]") == "true"


def test_second_nonblank_line_does_not_fire():
    # Must survive: the fix must not accept a genuinely multi-line body whose
    # first line is the trigger — that is the loop-prevention boundary.
    assert run_verify_invocation(
        "issue_comment", "@claude review\nplease also fix the flaky test", "claude[bot]"
    ) == "false"


def test_multiline_review_output_quoting_claude_does_not_fire():
    body = "@claude review\n\nLGTM\n### Recommended Optional\n1. Something to consider."
    assert run_verify_invocation("issue_comment", body, "claude[bot]") == "false"


def test_bot_non_review_comment_does_not_fire():
    assert run_verify_invocation("issue_comment", "@claude fix this", "claude[bot]") == "false"


def test_human_at_claude_invocation_fires():
    assert run_verify_invocation("issue_comment", "@claude fix this", "someuser") == "true"


def test_human_at_claude_only_in_code_block_does_not_fire():
    body = "here is an example:\n```\n@claude review\n```\nthanks"
    assert run_verify_invocation("issue_comment", body, "someuser") == "false"
