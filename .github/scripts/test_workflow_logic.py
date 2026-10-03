
import os
import re
import subprocess
import tempfile

import pytest

HERE = os.path.dirname(__file__)
CLAUDE_YML = os.path.abspath(os.path.join(HERE, "..", "workflows", "claude.yml"))

VERIFY_STEP = "Verify @claude is an actual invocation (not in a code block or example)"
CLASSIFY_MODE_STEP = "Classify invocation route (review, implement, or fix-pr)"

PR_URL = "https://api.github.com/repos/o/r/pulls/5"


def _read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def extract_step_run_block(yml_text, step_name):
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


CLASSIFY_CASES = [
    ("trusted_member_no_review_word", "issue_comment", "@claude correct the lint error", PR_URL, "MEMBER", "", "", "fix-pr"),
    ("trusted_owner_pr_comment", "issue_comment", "@claude address the feedback", PR_URL, "OWNER", "", "", "fix-pr"),
    ("claude_bot_authored_pr", "issue_comment", "@claude address the feedback", PR_URL, "NONE", "claude[bot]", "", "fix-pr"),
    ("external_author_pr_comment", "issue_comment", "@claude fix the lint error", PR_URL, "NONE", "", "", "review"),
    ("contributor_author_pr_comment", "issue_comment", "@claude fix this", PR_URL, "CONTRIBUTOR", "", "", "review"),
    ("review_keyword_trusted_author", "issue_comment", "@claude review this carefully", PR_URL, "MEMBER", "", "", "review"),
    ("review_keyword_after_model_shorthand", "issue_comment", "@claude sonnet review", PR_URL, "MEMBER", "", "", "review"),
    ("review_and_fix_loses_push", "issue_comment", "@claude review and fix it", PR_URL, "OWNER", "", "", "review"),
    ("review_word_later_in_sentence", "issue_comment", "@claude fix the review comments", PR_URL, "MEMBER", "", "", "fix-pr"),
    ("fix_keyword", "issue_comment", "@claude fix", PR_URL, "MEMBER", "", "", "fix-pr"),
    ("fix_keyword_after_model_shorthand", "issue_comment", "@claude opus fix and be thorough", PR_URL, "OWNER", "", "", "fix-pr"),
    ("old_fix_pr_spelling", "issue_comment", "@claude fix-pr", PR_URL, "MEMBER", "", "", "fix-pr"),
    ("fix_keyword_untrusted_pr_author", "issue_comment", "@claude fix", PR_URL, "NONE", "", "", "review"),
    ("fix_keyword_on_plain_issue", "issue_comment", "@claude fix", "", "MEMBER", "", "", "implement"),
    ("fix_keyword_on_inline_review_surface", "pull_request_review_comment", "@claude fix", PR_URL, "OWNER", "", "", "review"),
    ("pull_request_review_surface", "pull_request_review", "@claude fix this", PR_URL, "MEMBER", "", "", "review"),
    ("pull_request_review_comment_surface", "pull_request_review_comment", "@claude fix this", PR_URL, "OWNER", "", "", "review"),
    ("issues_event", "issues", "@claude build this feature", "", "MEMBER", "", "", "implement"),
    ("docs_release_flow_on_pr", "issue_comment", "@claude sync-docs", PR_URL, "MEMBER", "", "sync-docs", "implement"),
    ("issue_comment_on_issue", "issue_comment", "@claude implement this", "", "MEMBER", "", "", "implement"),
]


@pytest.mark.parametrize(
    "event_name,stripped,pr_url,pr_author_assoc,pr_author_login,flow,expected",
    [c[1:] for c in CLASSIFY_CASES],
    ids=[c[0] for c in CLASSIFY_CASES],
)
def test_classify_mode_routes(event_name, stripped, pr_url, pr_author_assoc, pr_author_login, flow, expected):
    assert run_classify_mode(
        event_name,
        stripped,
        pr_url=pr_url,
        pr_author_assoc=pr_author_assoc,
        pr_author_login=pr_author_login,
        flow=flow,
    ) == expected


VERIFY_CASES = [
    ("exact_one_line_self_trigger", "issue_comment", "@claude review", "claude[bot]", "true"),
    ("leading_blank_line", "issue_comment", "\n@claude review", "claude[bot]", "true"),
    ("leading_blank_and_indentation", "issue_comment", "  \n   @claude review  ", "claude[bot]", "true"),
    ("trailing_carriage_return", "issue_comment", "@claude review\r", "claude[bot]", "true"),
    ("model_shorthand_self_trigger", "issue_comment", "@claude opus review", "claude[bot]", "true"),
    ("effort_token_self_trigger", "issue_comment", "@claude review effort:high", "claude[bot]", "true"),
    ("second_nonblank_line", "issue_comment", "@claude review\nplease also fix the flaky test", "claude[bot]", "false"),
    ("multiline_review_output_quoting_claude", "issue_comment", "@claude review\n\nLGTM\n### Recommended Optional\n1. Something to consider.", "claude[bot]", "false"),
    ("bot_non_review_comment", "issue_comment", "@claude fix this", "claude[bot]", "false"),
    ("human_at_claude_invocation", "issue_comment", "@claude fix this", "someuser", "true"),
    ("human_at_claude_only_in_code_block", "issue_comment", "here is an example:\n```\n@claude review\n```\nthanks", "someuser", "false"),
]


@pytest.mark.parametrize(
    "event_name,body,trigger_actor,expected",
    [c[1:] for c in VERIFY_CASES],
    ids=[c[0] for c in VERIFY_CASES],
)
def test_verify_invocation_fires(event_name, body, trigger_actor, expected):
    assert run_verify_invocation(event_name, body, trigger_actor) == expected
