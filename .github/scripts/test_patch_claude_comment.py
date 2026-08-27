from __future__ import annotations
import json
import os
import stat
import subprocess
from dataclasses import dataclass
from pathlib import Path
REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / '.github' / 'scripts' / 'patch_claude_comment.sh'
COMMENTS_PAGE = [[{'id': 101, 'user': {'login': 'claude[bot]'}, 'updated_at': '2026-07-01T00:00:00Z', 'body': 'review body from claude[bot]\n[View job](https://github.com/richkuo/go-trader/actions/runs/111)'}, {'id': 202, 'user': {'login': 'github-actions[bot]'}, 'updated_at': '2026-07-01T01:00:00Z', 'body': 'review body from github-actions[bot]\n[View job](https://github.com/richkuo/go-trader/actions/runs/222)'}, {'id': 303, 'user': {'login': 'richkuo'}, 'updated_at': '2026-07-01T02:00:00Z', 'body': 'human comment'}, {'id': 404, 'user': {'login': 'github-actions[bot]'}, 'updated_at': '2026-07-01T03:00:00Z', 'body': 'unrelated workflow comment, newer than the review comment\n[Nightly report](https://github.com/richkuo/go-trader/actions/runs/999)'}]]
FAKE_GH = '#!/usr/bin/env bash\n# Fake gh for tests: a --paginate fetch prints the canned comments page; a\n# --method call (PATCH/POST) records its argv (one arg per line) and prints\n# nothing; a bare `gh api repos/.../issues/comments/<id>` GET (the\n# TARGET_COMMENT_ID path) prints the canned single comment.\nset -euo pipefail\ncase " $* " in\n  *" --paginate "*)\n    cat "$GH_STUB_COMMENTS"\n    ;;\n  *" --method "*)\n    printf \'%s\\n\' "$@" >> "$GH_STUB_PATCH_LOG"\n    ;;\n  *)\n    cat "${GH_STUB_SINGLE:-/dev/null}"\n    ;;\nesac\n'

@dataclass(frozen=True)
class PatchScriptResult:
    stdout: str
    stderr: str
    log: str

    def assert_log_contains(self, needle: str) -> None:
        assert needle in self.log, f'expected {needle!r} in patch log, got {self.log!r}; stdout={self.stdout!r} stderr={self.stderr!r}'

    def assert_clean_noop(self, bot_login: str) -> None:
        assert self.log == '', f'expected empty patch log on noop, got {self.log!r}; stdout={self.stdout!r} stderr={self.stderr!r}'
        msg = f'No {bot_login} comment found — nothing to update.'
        assert msg in self.stdout, f'expected noop message {msg!r} in stdout, got {self.stdout!r}; stderr={self.stderr!r}'

def run_patch_script(tmp_path, extra_env, single_comment=None) -> PatchScriptResult:
    bin_dir = tmp_path / 'bin'
    bin_dir.mkdir()
    gh = bin_dir / 'gh'
    gh.write_text(FAKE_GH)
    gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
    comments = tmp_path / 'comments.json'
    comments.write_text(json.dumps(COMMENTS_PAGE))
    patch_log = tmp_path / 'patch.log'
    env = os.environ.copy()
    env.update({'PATH': f"{bin_dir}:{env['PATH']}", 'GH_STUB_COMMENTS': str(comments), 'GH_STUB_PATCH_LOG': str(patch_log), 'REPO': 'richkuo/go-trader', 'ISSUE_NUMBER': '1178', 'GH_TOKEN': 'test-token', 'MODEL_ID': 'claude-sonnet-5', 'EFFORT': 'xhigh', 'CLAUDE_HARNESS': 'anthropics/claude-code-action@v1'})
    if single_comment is not None:
        single = tmp_path / 'single.json'
        single.write_text(json.dumps(single_comment))
        env['GH_STUB_SINGLE'] = str(single)
    env.update(extra_env)
    result = subprocess.run(['bash', str(SCRIPT)], cwd=REPO_ROOT, env=env, capture_output=True, text=True)
    assert result.returncode == 0, f'patch script exited {result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}'
    log = patch_log.read_text() if patch_log.exists() else ''
    return PatchScriptResult(stdout=result.stdout, stderr=result.stderr, log=log)

def test_default_bot_login_patches_claude_bot_comment(tmp_path):
    patched = run_patch_script(tmp_path, {})
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/101')
    patched.assert_log_contains('body from claude[bot]')

def test_bot_login_override_without_run_id_takes_latest_by_author(tmp_path):
    patched = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]'})
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/404')

def test_run_id_selects_own_comment_despite_newer_same_author(tmp_path):
    patched = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '222'})
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/202')
    patched.assert_log_contains('body from github-actions[bot]')

def test_run_id_without_match_is_a_clean_noop(tmp_path):
    run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '555'}).assert_clean_noop('github-actions[bot]')

def test_run_id_match_is_not_a_prefix_match(tmp_path):
    run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '22'}).assert_clean_noop('github-actions[bot]')

def test_run_id_also_constrains_default_claude_bot(tmp_path):
    patched = run_patch_script(tmp_path, {'RUN_ID': '111'})
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/101')

def test_no_matching_comment_is_a_clean_noop(tmp_path):
    run_patch_script(tmp_path, {'BOT_LOGIN': 'nobody[bot]'}).assert_clean_noop('nobody[bot]')

def test_on_miss_post_creates_new_status_comment(tmp_path):
    patched = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '555', 'ON_MISS': 'post', 'STATUS_NOTE': '**Workflow failed before completion.** See run log.'})
    patched.assert_log_contains('--method\nPOST')
    patched.assert_log_contains('repos/richkuo/go-trader/issues/1178/comments')
    patched.assert_log_contains('Workflow failed before completion')
    assert 'PATCH' not in patched.log, f'unexpected PATCH in log={patched.log!r}; stdout={patched.stdout!r} stderr={patched.stderr!r}'

def test_on_miss_post_still_patches_when_own_comment_exists(tmp_path):
    patched = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '222', 'ON_MISS': 'post', 'STATUS_NOTE': '**Workflow failed before completion.** See run log.'})
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/202')
    assert '--method\nPOST' not in patched.log, f'unexpected POST in log={patched.log!r}; stdout={patched.stdout!r} stderr={patched.stderr!r}'

def test_on_miss_post_without_status_note_is_a_noop(tmp_path):
    run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '555', 'ON_MISS': 'post'}).assert_clean_noop('github-actions[bot]')

def test_select_only_emits_run_matched_comment_id_without_patching(tmp_path):
    result = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '222', 'SELECT_ONLY': '1'})
    assert result.stdout == '202', f"expected stdout '202', got {result.stdout!r}; stderr={result.stderr!r}"
    assert result.log == '', f'expected empty patch log, got {result.log!r}; stdout={result.stdout!r} stderr={result.stderr!r}'

def test_select_only_emits_empty_string_on_miss(tmp_path):
    result = run_patch_script(tmp_path, {'BOT_LOGIN': 'github-actions[bot]', 'RUN_ID': '555', 'SELECT_ONLY': '1'})
    assert result.stdout == '', f'expected empty stdout, got {result.stdout!r}; stderr={result.stderr!r}'
    assert result.log == '', f'expected empty patch log, got {result.log!r}; stdout={result.stdout!r} stderr={result.stderr!r}'

def test_target_comment_id_patches_that_comment_bypassing_selection(tmp_path):
    single = {'id': 202, 'user': {'login': 'claude[bot]'}, 'updated_at': '2026-07-01T01:00:00Z', 'body': 'primary work comment from claude[bot]'}
    patched = run_patch_script(tmp_path, {'TARGET_COMMENT_ID': '202'}, single_comment=single)
    patched.assert_log_contains('repos/richkuo/go-trader/issues/comments/202')
    patched.assert_log_contains('primary work comment')
    patched.assert_log_contains('--method\nPATCH')
    assert 'issues/comments/101' not in patched.log, f'unexpected comment 101 in log={patched.log!r}; stdout={patched.stdout!r} stderr={patched.stderr!r}'

def test_fake_gh_stub_has_no_printf_grep_pipelines():
    assert '| grep' not in FAKE_GH
    assert 'grep -q' not in FAKE_GH
    assert 'case " $* "' in FAKE_GH
