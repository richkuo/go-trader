#!/usr/bin/env bash
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
tmp="$(mktemp -d)" || exit 2
trap 'rm -rf "$tmp"' EXIT
fail=0
n=0
sources=(CLAUDE.md AGENTS.md SKILL.md README.md backtest/AUDIT.md docs/backtesting-registry.md docs/POST_UPDATE_HISTORY.md
  scripts/check_pre_pr.sh scripts/agent_guide.py scripts/defect_audit.py)
while IFS= read -r f; do
  sources+=("$f")
done < <(git -C "$root" ls-files .claude)

g() { git -C "$repo" -c user.email=ci@example.invalid -c user.name=ci -c init.defaultBranch=main "$@"; }

fresh() {
  repo="$tmp/case$n"
  mkdir -p "$repo"
  local f
  for f in "${sources[@]}"; do
    mkdir -p "$repo/$(dirname "$f")"
    cp -P "$root/$f" "$repo/$f" || return 1
  done
  git init -q "$repo"
  g add -A
  g commit -q -m base
}

regen() { python3 -I "$repo/scripts/agent_guide.py" >/dev/null; }

run_check() { (cd "$repo" && bash scripts/check_pre_pr.sh agent-docs) >"$tmp/out" 2>&1; }

tree_hash() { (cd "$repo" && find . -path ./.git -prune -o -type f -print0 -o -type l -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) | shasum -a 256; }

expect() {
  local want="$1" name="$2" pattern="${3:-}" got
  n=$((n + 1))
  if run_check; then got=pass; else got=fail; fi
  if [ "$got" != "$want" ]; then
    echo "FAIL: $name: agent-docs check gave $got, want $want"
    sed 's/^/  | /' "$tmp/out"
    fail=1
  elif [ -n "$pattern" ] && ! grep -qF -- "$pattern" "$tmp/out"; then
    echo "FAIL: $name: output lacks: $pattern"
    sed 's/^/  | /' "$tmp/out"
    fail=1
  else
    echo "PASS: $name ($want)"
  fi
}

case_start() { fresh || { echo "FAIL: fixture copy"; exit 1; }; }

pad_lines() { python3 -I -c 'import sys; p, target = sys.argv[1], int(sys.argv[2]); t = open(p).read(); line = "- pad " + "x" * 60 + "\n"; t += line * ((target - len(t.encode())) // len(line)); t += "-" + " " * (target - len(t.encode()) - 2) + "\n" if target - len(t.encode()) >= 2 else ""; open(p, "w").write(t)' "$@"; }

rule() { printf -- '---\npaths:\n  - "%s"\n---\n\n## %s\n\n- %s\n' "$2" "$3" "$4" >"$repo/.claude/rules/$1"; }

manifest_add() { python3 -I -c 'import sys; p = sys.argv[1]; t = open(p).read(); a = "## Area rules\n"; assert a in t; open(p, "w").write(t.replace(a, a + "- `.claude/rules/" + sys.argv[2] + "`:fixture\n", 1))' "$repo/CLAUDE.md" "$1"; }

case_start
before="$(tree_hash)"
expect pass "pristine copy of the real sources"
after="$(tree_hash)"
n=$((n + 1))
if [ "$before" = "$after" ] && [ -z "$(g status --porcelain)" ]; then
  echo "PASS: --check leaves the tree byte-identical"
else
  echo "FAIL: --check changed the tree"
  fail=1
fi

case_start
mkdir -p "$repo/.claude/hooks"
printf '{}\n' >"$repo/.claude/settings.json"
printf 'x = 1\n' >"$repo/.claude/hooks/command_policy.py"
g add -A
expect pass "tracked .claude/settings.json and .claude/hooks/command_policy.py are allowed"

case_start
first="$(git -C "$repo" ls-files .claude/rules | head -n 1)"
printf -- '- fixture edit\n' >>"$repo/$first"
before="$(tree_hash)"
expect fail "rule file edited with no regeneration" "AGENTS.md is stale"
n=$((n + 1))
if [ "$before" = "$(tree_hash)" ]; then
  echo "PASS: --check on a stale tree writes nothing"
else
  echo "FAIL: --check on a stale tree changed the tree"
  fail=1
fi

case_start
printf -- '- fixture edit\n' >>"$repo/CLAUDE.md"
expect fail "CLAUDE.md edited with no regeneration" "AGENTS.md is stale"

case_start
printf 'hand edit\n' >>"$repo/AGENTS.md"
expect fail "hand-edited AGENTS.md" "AGENTS.md is stale"

case_start
rm "$repo/AGENTS.md"
ln -s CLAUDE.md "$repo/AGENTS.md"
g add -A
expect fail "AGENTS.md as a symlink to CLAUDE.md" "not a symlink"

case_start
rm "$repo/AGENTS.md"
g add -A
expect fail "AGENTS.md missing" "AGENTS.md is missing"

case_start
manifest_add zz-missing.md
expect fail "CLAUDE.md lists a missing rule file" "zz-missing.md: CLAUDE.md lists it, but the file is missing"

case_start
rule zz-extra.md "zz/**" "Extra" "fixture rule"
g add -A
expect fail "rule file absent from the CLAUDE.md list" "zz-extra.md: not listed"

case_start
manifest_add zz-untracked.md
rule zz-untracked.md "zz/**" "Untracked" "fixture rule"
regen
g add CLAUDE.md AGENTS.md
expect fail "untracked rule file" "zz-untracked.md: not tracked by git"

case_start
manifest_add zz-big.md
rule zz-big.md "zz/**" "Big" "fixture rule"
pad_lines "$repo/.claude/rules/zz-big.md" 8001
g add -A
expect fail "rule file over 8000 bytes" "above the 8000-byte limit"

case_start
for i in 1 2 3 4; do
  manifest_add "zz-fill-$i.md"
  rule "zz-fill-$i.md" "zz/**" "Fill $i" "fixture rule"
  pad_lines "$repo/.claude/rules/zz-fill-$i.md" 7900
done
g add -A
expect fail "AGENTS.md over 28000 bytes" "above the 28000-byte limit"

case_start
pad_lines "$repo/CLAUDE.md" 17500
regen
g add -A
expect fail "CLAUDE.md at 17500 bytes" "at or above the 17500-byte ceiling"

case_start
python3 -I -c 'import sys; p = sys.argv[1]; open(p, "a").write("- " + "y" * 399 + "\n")' "$repo/CLAUDE.md"
expect fail "401-byte bullet in CLAUDE.md" "line is 401 bytes, above the 400-byte limit"

case_start
manifest_add zz-long.md
rule zz-long.md "zz/**" "Long" "$(printf 'y%.0s' $(seq 1 399))"
g add -A
expect fail "401-byte bullet in a rule file" "line is 401 bytes, above the 400-byte limit"

case_start
manifest_add zz-inline.md
printf -- '---\npaths: ["zz/**"]\n---\n\n## Inline\n\n- fixture rule\n' >"$repo/.claude/rules/zz-inline.md"
g add -A
expect fail "malformed frontmatter" "frontmatter must start with"

case_start
manifest_add zz-nopaths.md
printf -- '---\npaths:\n---\n\n## No paths\n\n- fixture rule\n' >"$repo/.claude/rules/zz-nopaths.md"
g add -A
expect fail "frontmatter with no paths" "paths: lists no glob"

case_start
manifest_add zz-noslash.md
rule zz-noslash.md "*.go" "No slash" "fixture rule"
g add -A
expect fail "path with no slash" "has no slash"

case_start
manifest_add zz-link.md
ln -s "$first" "$repo/.claude/rules/zz-link.md"
g add -A
expect fail "rule file as a symlink" "zz-link.md: must be a regular file"

case_start
mkdir -p "$repo/.claude/rules/sub"
printf 'x\n' >"$repo/.claude/rules/sub/x.md"
g add -A
expect fail "subdirectory under .claude/rules" "sub: must be a regular file"

case_start
mkdir -p "$repo/scheduler"
printf 'x\n' >"$repo/scheduler/CLAUDE.md"
g add -A
expect fail "tracked scheduler/CLAUDE.md" "scheduler/CLAUDE.md"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = Position{}.HedgeFor\n' >"$repo/scheduler/zz_hedge_probe.go"
g add -A
expect fail "scheduler file names a hedge with no hedge.md glob" "hedge.md: scheduler/zz_hedge_probe.go names a hedge but matches no paths: glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = Position{}.HedgeFor\n' >"$repo/scheduler/zz_hedge_probe_test.go"
g add -A
expect pass "hedge name in a scheduler test file needs no hedge.md glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = Position{}.HedgeFor\n' >"$repo/scheduler/hedge_zz_probe.go"
g add -A
expect pass "new scheduler/hedge*.go file is covered by the hedge.md glob"

case_start
mkdir -p "$repo/shared_tools"
printf 'import os\n\nd = os.environ["GO_TRADER_ROBINHOOD_SESSION_DIR"]\n' >"$repo/shared_tools/zz_session_probe.py"
g add -A
expect fail "file names the Robinhood session directory with no robinhood.md glob" "robinhood.md: shared_tools/zz_session_probe.py names GO_TRADER_ROBINHOOD_SESSION_DIR but matches no paths: glob"

case_start
mkdir -p "$repo/scripts"
printf 'update_git status\n' >"$repo/scripts/zz_deploy_probe.sh"
g add -A
expect fail "script names update_git with no deploy.md glob" "deploy.md: scripts/zz_deploy_probe.sh names update_git or update_resolve_tool but matches no paths: glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = opsCommandNames\n' >"$repo/scheduler/zz_ops_probe.go"
g add -A
expect fail "scheduler file names opsCommandNames with no discord.md glob" "discord.md: scheduler/zz_ops_probe.go names opsCommandNames or slashCommands( but matches no paths: glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = opsCommandNames\n' >"$repo/scheduler/zz_ops_probe_test.go"
g add -A
expect pass "opsCommandNames in a scheduler test file needs no discord.md glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = hlInfoPost\n' >"$repo/scheduler/zz_info_probe.go"
g add -A
expect fail "scheduler file names hlInfoPost with no market-feed.md glob" "market-feed.md: scheduler/zz_info_probe.go names hlInfoPost, feedMu or weight_reserve but matches no paths: glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = LoadConfigReadOnly\n' >"$repo/scheduler/zz_merge_probe.go"
g add -A
expect fail "scheduler file names LoadConfigReadOnly with no merge-paper.md glob" "merge-paper.md: scheduler/zz_merge_probe.go names LoadConfigReadOnly or resolveChannel( but matches no paths: glob"

case_start
mkdir -p "$repo/scheduler"
printf 'package main\n\nvar zz = uiPartitionParam\n' >"$repo/scheduler/zz_ui_probe.go"
g add -A
expect fail "scheduler file names uiPartitionParam with no ui.md glob" "ui.md: scheduler/zz_ui_probe.go names uiPartitionParam or spawnPythonProcessWithEnv but matches no paths: glob"

case_start
mkdir -p "$repo/docs/test-reduction"
printf 'LoadConfigReadOnly update_git opsCommandNames\n' >"$repo/docs/test-reduction/zz-suite.jsonl"
g add -A
expect pass "guarded names in a docs data file need no glob"

case_start
g rm -q .claude/rules/hedge.md
python3 -I -c 'import sys; p = sys.argv[1]; t = open(p).read(); open(p, "w").write("".join(l for l in t.splitlines(True) if "rules/hedge.md" not in l))' "$repo/CLAUDE.md"
regen
g add -A
expect fail "hedge.md removed with its coverage check still required" "hedge.md: required rule file is missing"

case_start
printf 'x\n' >"$repo/AGENTS.override.md"
g add -A
expect fail "tracked AGENTS.override.md" "AGENTS.override.md"

case_start
mkdir -p "$repo/scheduler/.claude/rules"
printf 'x\n' >"$repo/scheduler/.claude/rules/x.md"
g add -A
expect fail "tracked nested scheduler/.claude/rules/x.md" "scheduler/.claude/rules/x.md"

case_start
mkdir -p "$repo/.claude/hooks"
printf 'x = 1\n' >"$repo/.claude/hooks/other.py"
g add -A
expect fail "tracked .claude/hooks/other.py" ".claude/hooks/other.py"

case_start
manifest_add zz-guard.md
rule zz-guard.md "zz/**" "Guard" '`fixture_only_guard.go`:fixture rule'
regen
g add -A
n=$((n + 1))
if (cd "$repo" && python3 -I scripts/defect_audit.py guard-set) >"$tmp/out" 2>&1 && grep -qF '"fixture_only_guard.go"' "$tmp/out" && ! grep -qF fixture_only_guard "$repo/CLAUDE.md"; then
  echo "PASS: the test-pruning audit protects a glob that only a rule file names"
else
  echo "FAIL: the test-pruning audit misses a glob that only a rule file names"
  sed 's/^/  | /' "$tmp/out"
  fail=1
fi

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "PASS: agent docs fixtures ($n checks: the pristine sources and the allowed .claude files pass; stale, hand-edited, symlinked or missing AGENTS.md, a listed but missing, unlisted, untracked, oversize, symlinked or nested rule file, bad frontmatter, a slash-less path, an oversize AGENTS.md, a CLAUDE.md at the ceiling, a 401-byte line, an extra instruction file and an unlisted .claude file fail; --check writes nothing; the audit reads rule files)"
