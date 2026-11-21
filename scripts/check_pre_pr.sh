#!/usr/bin/env bash
set -u

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
cd "$root" || exit 2

checks="syntax test-budget agent-docs agent-docs-fixtures doc-lines github-scripts"

usage() {
  echo "usage: bash scripts/check_pre_pr.sh [all|syntax|test-budget|agent-docs|agent-docs-fixtures|doc-lines|github-scripts]" >&2
}

check_syntax() {
  find shared_scripts/ platforms/ shared_strategies/ shared_tools/ \
    -name '*.py' -print0 \
    | xargs -0 uv run --no-sync python3 -m py_compile
}

check_test_budget() {
  uv run --no-sync python3 scripts/check_test_budget.py
}

check_agent_docs() {
  fail=0
  if [ -L CLAUDE.md ] || [ ! -f CLAUDE.md ]; then
    echo "::error::CLAUDE.md must be a regular file at the repository root."
    fail=1
  fi
  if [ -L AGENTS.md ] || [ ! -f AGENTS.md ]; then
    echo "::error::AGENTS.md must be the generated regular file at the repository root, not a symlink. Run: python3 -I scripts/agent_guide.py"
    fail=1
  fi
  extra=$(git ls-files | grep -E '(^|/)(CLAUDE(\.local)?\.md|AGENTS(\.override)?\.md|\.cursorrules|copilot-instructions\.md)$|(^|/)\.cursor/rules/|.+/\.claude/' | grep -vx -e 'CLAUDE.md' -e 'AGENTS.md' || true)
  if [ -n "$extra" ]; then
    echo "::error::Agent instructions stay in the root CLAUDE.md, the .claude/rules files it lists, and the generated AGENTS.md. Remove these files:"
    echo "$extra"
    fail=1
  fi
  dot=$(git ls-files .claude | grep -vxE '\.claude/rules/[a-z0-9]+(-[a-z0-9]+)*\.md|\.claude/skills/go-trader-trade-feedback/(SKILL\.md|trade_feedback_client\.py)|\.claude/settings\.json|\.claude/hooks/command_policy\.py' || true)
  if [ -n "$dot" ]; then
    echo "::error::Tracked files under .claude/ must be area rules (.claude/rules/<name>.md), the go-trader-trade-feedback skill files, .claude/settings.json or .claude/hooks/command_policy.py. Remove these files:"
    echo "$dot"
    fail=1
  fi
  if [ -f CLAUDE.md ] && [ ! -L CLAUDE.md ]; then
    bytes=$(wc -c < CLAUDE.md | tr -d ' ')
    echo "CLAUDE.md: $bytes bytes (ceiling 17500, target 17000)"
    if [ "$bytes" -ge 17500 ]; then
      echo "::error::CLAUDE.md is $bytes bytes, at or above the 17500-byte ceiling. Move mechanism to SKILL.md, move an area rule to .claude/rules, shorten wording, drop no guardrail."
      echo "Bytes per section:"
      LC_ALL=C awk 'BEGIN{h="(header)"} /^## /{printf "%6d  %s\n",n,h; h=$0; n=0} {n+=length($0)+1} END{printf "%6d  %s\n",n,h}' CLAUDE.md
      fail=1
    elif [ "$bytes" -gt 17000 ]; then
      echo "::warning::CLAUDE.md is $bytes bytes, above the 17000-byte target. The CI check fails at 17500."
    fi
  fi
  if ! python3 -I scripts/agent_guide.py --check; then
    fail=1
  fi
  return $fail
}

check_agent_docs_fixtures() {
  bash scripts/check_agent_docs_fixtures.sh
}

check_doc_lines() {
  limit=1000
  LC_ALL=C awk -v limit="$limit" '
    FNR == 1 { fence = 0 }
    match($0, /^ *```/) && RLENGTH <= 6 { fence = !fence; next }
    fence { next }
    $0 ~ /^[ \t]*\|/ { next }
    length($0) > limit {
      printf "::error file=%s,line=%d::line is %d bytes, above the %d-byte limit. Put one sentence per line.\n", FILENAME, FNR, length($0), limit
      bad = 1
    }
    END { exit bad }
  ' SKILL.md docs/POST_UPDATE_HISTORY.md
}

check_github_scripts() {
  files=$(git ls-files .github/scripts)
  if [ "$files" != ".github/scripts/test_workflow_logic.py" ]; then
    echo "::error::.github/scripts must hold exactly test_workflow_logic.py. Put other scripts under scripts/. Tracked files:"
    echo "$files"
    return 1
  fi
}

run_one() {
  case "$1" in
    syntax) (check_syntax) ;;
    test-budget) (check_test_budget) ;;
    agent-docs) (check_agent_docs) ;;
    agent-docs-fixtures) (check_agent_docs_fixtures) ;;
    doc-lines) (check_doc_lines) ;;
    github-scripts) (check_github_scripts) ;;
    *) usage; return 2 ;;
  esac
}

if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi

target="${1:-all}"
if [ "$target" != all ]; then
  run_one "$target"
  exit $?
fi

failed=""
summary=""
for name in $checks; do
  echo "== $name"
  if run_one "$name"; then
    summary="$summary
PASS: $name"
  else
    summary="$summary
FAIL: $name"
    failed="$failed $name"
  fi
done

echo "== summary$summary"
if [ -n "$failed" ]; then
  echo "check_pre_pr: failed:$failed"
  exit 1
fi
echo "check_pre_pr: all checks passed"
: hosted-proof-2131
