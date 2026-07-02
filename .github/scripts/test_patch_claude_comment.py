"""Tests for patch_claude_comment.sh — bot-login + run-id selection (#1178).

The review job binds the agent to the job-scoped token, so its comments post
as github-actions[bot] instead of claude[bot]. The patch script must select
the latest comment authored by $BOT_LOGIN (default claude[bot]) rather than a
hard-coded login. github-actions[bot] is a shared author (any workflow can
post as it), so when $RUN_ID is set the selection is further constrained to
the action's own tracking comment, which embeds the /actions/runs/<run_id>
link — never another workflow's comment. gh is stubbed with a fake executable
on PATH; the script's comment recomposition (compose_claude_comment.py) runs
for real.
"""

import json
import os
import stat
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / ".github" / "scripts" / "patch_claude_comment.sh"

COMMENTS_PAGE = [
    [
        {
            "id": 101,
            "user": {"login": "claude[bot]"},
            "updated_at": "2026-07-01T00:00:00Z",
            "body": "review body from claude[bot]\n"
            "[View job](https://github.com/richkuo/go-trader/actions/runs/111)",
        },
        {
            "id": 202,
            "user": {"login": "github-actions[bot]"},
            "updated_at": "2026-07-01T01:00:00Z",
            "body": "review body from github-actions[bot]\n"
            "[View job](https://github.com/richkuo/go-trader/actions/runs/222)",
        },
        {
            "id": 303,
            "user": {"login": "richkuo"},
            "updated_at": "2026-07-01T02:00:00Z",
            "body": "human comment",
        },
        {
            "id": 404,
            "user": {"login": "github-actions[bot]"},
            "updated_at": "2026-07-01T03:00:00Z",
            "body": "unrelated workflow comment, newer than the review comment\n"
            "[Nightly report](https://github.com/richkuo/go-trader/actions/runs/999)",
        },
    ]
]

FAKE_GH = """#!/usr/bin/env bash
# Fake gh for tests: --paginate fetch prints the canned comments page;
# the PATCH call records its argv (one arg per line) and prints nothing.
set -euo pipefail
if printf '%s\\n' "$@" | grep -q -- '--paginate'; then
  cat "$GH_STUB_COMMENTS"
else
  printf '%s\\n' "$@" >> "$GH_STUB_PATCH_LOG"
fi
"""


def run_patch_script(tmp_path, extra_env):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    gh = bin_dir / "gh"
    gh.write_text(FAKE_GH)
    gh.chmod(gh.stat().st_mode | stat.S_IEXEC)

    comments = tmp_path / "comments.json"
    comments.write_text(json.dumps(COMMENTS_PAGE))
    patch_log = tmp_path / "patch.log"

    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{bin_dir}:{env['PATH']}",
            "GH_STUB_COMMENTS": str(comments),
            "GH_STUB_PATCH_LOG": str(patch_log),
            "REPO": "richkuo/go-trader",
            "ISSUE_NUMBER": "1178",
            "GH_TOKEN": "test-token",
            "MODEL_ID": "claude-sonnet-5",
            "EFFORT": "xhigh",
            "CLAUDE_HARNESS": "anthropics/claude-code-action@v1",
        }
    )
    env.update(extra_env)

    result = subprocess.run(
        ["bash", str(SCRIPT)],
        cwd=REPO_ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    return patch_log.read_text() if patch_log.exists() else ""


def test_default_bot_login_patches_claude_bot_comment(tmp_path):
    patched = run_patch_script(tmp_path, {})
    assert "repos/richkuo/go-trader/issues/comments/101" in patched
    assert "body from claude[bot]" in patched


def test_bot_login_override_without_run_id_takes_latest_by_author(tmp_path):
    # Documents the collision the RUN_ID filter exists to prevent: without it,
    # the newest github-actions[bot] comment wins even if another workflow
    # authored it.
    patched = run_patch_script(tmp_path, {"BOT_LOGIN": "github-actions[bot]"})
    assert "repos/richkuo/go-trader/issues/comments/404" in patched


def test_run_id_selects_own_comment_despite_newer_same_author(tmp_path):
    # Must survive: a second, unrelated github-actions[bot] comment updated
    # after the review comment (id 404, run 999) — the run-id filter pins the
    # selection to this run's tracking comment (id 202, run 222).
    patched = run_patch_script(
        tmp_path, {"BOT_LOGIN": "github-actions[bot]", "RUN_ID": "222"}
    )
    assert "repos/richkuo/go-trader/issues/comments/202" in patched
    assert "body from github-actions[bot]" in patched


def test_run_id_without_match_is_a_clean_noop(tmp_path):
    # Must survive: no comment from this run — never fall back to another
    # author-matching comment (that would stamp a foreign workflow's comment).
    patched = run_patch_script(
        tmp_path, {"BOT_LOGIN": "github-actions[bot]", "RUN_ID": "555"}
    )
    assert patched == ""


def test_run_id_match_is_not_a_prefix_match(tmp_path):
    # Run 22 must not match /actions/runs/222.
    patched = run_patch_script(
        tmp_path, {"BOT_LOGIN": "github-actions[bot]", "RUN_ID": "22"}
    )
    assert patched == ""


def test_run_id_also_constrains_default_claude_bot(tmp_path):
    patched = run_patch_script(tmp_path, {"RUN_ID": "111"})
    assert "repos/richkuo/go-trader/issues/comments/101" in patched


def test_no_matching_comment_is_a_clean_noop(tmp_path):
    patched = run_patch_script(tmp_path, {"BOT_LOGIN": "nobody[bot]"})
    assert patched == ""
