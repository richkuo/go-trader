"""Tests for patch_claude_comment.sh — bot-login selection (#1178).

The review job binds the agent to the job-scoped token, so its comments post
as github-actions[bot] instead of claude[bot]. The patch script must select
the latest comment authored by $BOT_LOGIN (default claude[bot]) rather than a
hard-coded login. gh is stubbed with a fake executable on PATH; the script's
comment recomposition (compose_claude_comment.py) runs for real.
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
            "body": "review body from claude[bot]",
        },
        {
            "id": 202,
            "user": {"login": "github-actions[bot]"},
            "updated_at": "2026-07-01T01:00:00Z",
            "body": "review body from github-actions[bot]",
        },
        {
            "id": 303,
            "user": {"login": "richkuo"},
            "updated_at": "2026-07-01T02:00:00Z",
            "body": "human comment",
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


def test_bot_login_override_patches_github_actions_comment(tmp_path):
    patched = run_patch_script(tmp_path, {"BOT_LOGIN": "github-actions[bot]"})
    assert "repos/richkuo/go-trader/issues/comments/202" in patched
    assert "body from github-actions[bot]" in patched


def test_no_matching_comment_is_a_clean_noop(tmp_path):
    patched = run_patch_script(tmp_path, {"BOT_LOGIN": "nobody[bot]"})
    assert patched == ""
