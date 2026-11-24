#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
detector="$root/scripts/ci_code_changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
outputs=(code quote egress_docker merge_paper update migrate)
gates=(quote egress_docker merge_paper update migrate)
manual_gates=(shared_feed)

g() { git -C "$tmp/repo" -c user.email=ci@example.invalid -c user.name=ci -c init.defaultBranch=main -c advice.detachedHead=false "$@"; }

git init -q "$tmp/repo"
g config core.fileMode true
mkdir -p "$tmp/repo/scheduler/cmd/egress-controller" "$tmp/repo/docs" "$tmp/repo/scripts/fixtures" "$tmp/repo/systemd" "$tmp/repo/.github/workflows" \
  "$tmp/repo/.claude/rules" "$tmp/repo/.claude/hooks"
for f in CLAUDE.md SKILL.md README.md scheduler/executor.go docs/backtesting-registry.md scheduler/cmd/egress-controller/manifest.go \
  scheduler/quote_coordinator.go scripts/merge-paper-instance.sh scripts/update.sh scripts/migrate-service-layout.py \
  scheduler/server.go scheduler/risk.go scheduler/state_presence.go scheduler/update_sh_feature_test.go \
  scheduler/main.go scheduler/probe_cmd.go scheduler/storage_inspect.go scheduler/singleton_lock.go scheduler/inspect_cmd.go \
  scheduler/market_feed_role.go scheduler/version.go .github/workflows/ci.yml scripts/shared-feed-convert.sh \
  scheduler/market_feed_ws.go scheduler/hyperliquid_balance.go scripts/fixtures/shared_feed_venue.py \
  scripts/test_migrate_service_layout_fixture.sh systemd/go-trader@.service scheduler/config_reload.go \
  .claude/rules/backtest.md scripts/agent_guide.py scripts/check_agent_docs_fixtures.sh .claude/settings.json .claude/hooks/command_policy.py; do
  printf 'base\n' >"$tmp/repo/$f"
done
go_src() {
  local path="$1"
  shift
  printf '%s\n' "$@" >"$path"
}
go_body='func TestBase() { _ = os.Args }'
for f in scheduler/probe_cmd_test.go scheduler/run_evidence_test.go scheduler/market_feed_ws_test.go \
  scheduler/egress_integration_test.go scheduler/quote_coordinator_test.go scheduler/config_reload_test.go \
  scheduler/cmd/egress-controller/manifest_test.go; do
  go_src "$tmp/repo/$f" 'package main' '' 'import "os"' '' "$go_body"
done
go_src "$tmp/repo/scheduler/run_evidence_block_test.go" '/* block comment */' 'package main' '' "$go_body"
go_src "$tmp/repo/scheduler/run_evidence_raw_test.go" '// see `x`' 'package main' '' "$go_body"
go_src "$tmp/repo/scheduler/run_evidence_nodecl_test.go" 'package main'
go_src "$tmp/repo/scheduler/run_evidence_alias_test.go" 'package main' '' 'import (' 'typeé "os"' ')' '' "$go_body"
g add -A
g commit -q -m base
base="$(g rev-parse HEAD)"

push_before=unset
push_forced=unset
fail_git=""
case_head=""
repo_dir="$tmp/repo"
mkdir -p "$tmp/bin"
{
  printf '#!/usr/bin/env bash\n'
  printf 'real=%q\n' "$(command -v git)"
  cat <<'SHIM'
if [ -n "${CI_CHECK_FAIL_GIT:-}" ]; then
  for a in "$@"; do
    if [ "$a" = "$CI_CHECK_FAIL_GIT" ]; then
      "$real" "$@"
      exit 1
    fi
  done
fi
exec "$real" "$@"
SHIM
} >"$tmp/bin/git"
chmod +x "$tmp/bin/git"

detect() {
  local event="$1" out="$tmp/output" log="$tmp/log" name lines want_warn warn got="" value="" before="$push_before"
  local envs=(EVENT_NAME="$event" GITHUB_OUTPUT="$out")
  case "$before" in
    base) before="$base" ;;
    head) before="$case_head" ;;
  esac
  if [ "$before" != unset ]; then
    envs+=(PUSH_BEFORE="$before")
  fi
  if [ "$push_forced" != unset ]; then
    envs+=(PUSH_FORCED="$push_forced")
  fi
  if [ -n "$fail_git" ]; then
    envs+=(CI_CHECK_FAIL_GIT="$fail_git" PATH="$tmp/bin:$PATH")
  fi
  : >"$out"
  (cd "$repo_dir" && env -u PUSH_BEFORE -u PUSH_FORCED -u CI_CHECK_FAIL_GIT "${envs[@]}" bash "$detector" >"$log")
  lines="$(wc -l <"$out" | tr -d ' ')"
  if [ "$lines" != "${#outputs[@]}" ]; then
    echo "bad: $lines output lines" && return
  fi
  for name in "${outputs[@]}"; do
    lines="$(grep -c "^$name=" "$out" || true)"
    if [ "$lines" != 1 ]; then
      echo "bad: $lines $name= lines" && return
    fi
    got="$got $name=$(sed -n "s/^$name=//p" "$out")"
  done
  for name in "${manual_gates[@]}"; do
    lines="$(grep -c "^$name=" "$out" || true)"
    if [ "$lines" != 0 ]; then
      echo "bad: the manual gate $name is a job output" && return
    fi
    value="$(sed -nE "s/^code=.* $name=(true|false)( .*)?\$/\1/p" "$log")"
    want_warn=0
    if [ "$event" = pull_request ] && [ "$value" = true ]; then
      want_warn=1
    fi
    warn="$(grep -c '^::warning title=Manual proof needed::' "$log" || true)"
    if [ "$warn" != "$want_warn" ]; then
      echo "bad: $warn manual proof warnings with $name=$value, want $want_warn" && return
    fi
    got="$got $name=$value"
  done
  echo "${got# }"
}

check() {
  local name="$1" event="$2" merge="$3" want="$4" edit="$5"
  g checkout -q -B "case" "$base"
  (cd "$tmp/repo" && eval "$edit")
  g add -A
  g commit -q -m "$name"
  local head
  head="$(g rev-parse HEAD)"
  case_head="$head"
  g checkout -q --detach "$base"
  if [ "$merge" = merge ]; then
    g merge -q --no-ff --no-edit "$head"
  else
    g checkout -q --detach "$head"
  fi
  local got
  got="$(detect "$event")"
  if [ "$got" = "$want" ]; then
    echo "ok: $name -> $got"
  else
    echo "FAIL: $name: want $want, got $got"
    fail=1
  fi
}

check_env() {
  push_before="$5"
  push_forced="$6"
  check "$1" "$2" "$3" "$4" "$7"
  push_before=unset
  push_forced=unset
}

with_fail_git() {
  fail_git="$1"
  shift
  "$@"
  fail_git=""
}

res() { echo "code=$1 quote=$2 egress_docker=$3 merge_paper=$4 update=$5 migrate=$6 shared_feed=$7"; }

all="code=true quote=true egress_docker=true merge_paper=true update=true migrate=true shared_feed=true"
code_only="code=true quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=false"
none="code=false quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=false"

check "docs only, pull request" pull_request merge "$none" 'echo x >>CLAUDE.md; echo x >>SKILL.md; echo x >>README.md'
check "docs and code, pull request" pull_request merge "$code_only" 'echo x >>CLAUDE.md; echo x >>scheduler/executor.go'
check "area rule file, pull request" pull_request merge "$none" 'echo x >>.claude/rules/backtest.md'
check "new area rule file, pull request" pull_request merge "$none" 'echo x >.claude/rules/new-area.md'
check "generated guide, pull request" pull_request merge "$none" 'echo x >AGENTS.md'
check "area rule file, guide and generated guide, pull request" pull_request merge "$none" 'echo x >>.claude/rules/backtest.md; echo x >>CLAUDE.md; echo x >AGENTS.md'
check "area rule file and code, pull request" pull_request merge "$code_only" 'echo x >>.claude/rules/backtest.md; echo x >>scheduler/executor.go'
check "guide generator, pull request" pull_request merge "$code_only" 'echo x >>scripts/agent_guide.py'
check "agent docs fixture script, pull request" pull_request merge "$code_only" 'echo x >>scripts/check_agent_docs_fixtures.sh'
check "Claude settings, pull request" pull_request merge "$code_only" 'echo x >>.claude/settings.json'
check "Claude hook, pull request" pull_request merge "$code_only" 'echo x >>.claude/hooks/command_policy.py'
check "nested area rule path, pull request" pull_request merge "$code_only" 'mkdir -p .claude/rules/sub && echo x >.claude/rules/sub/x.md'
check "non-Markdown area rule path, pull request" pull_request merge "$code_only" 'echo x >.claude/rules/x.sh'
check "uppercase or underscore area rule name, pull request" pull_request merge "$code_only" 'echo x >.claude/rules/New_Area.md'
check "docs only, push" push merge "$all" 'echo x >>SKILL.md'
check "docs only, no event" "" merge "$all" 'echo x >>SKILL.md'
check "test-read doc, pull request" pull_request merge "$code_only" 'echo x >>docs/backtesting-registry.md'
check "rename out of the list, pull request" pull_request merge "$code_only" 'git mv README.md README2.md'
check "code renamed into the list, pull request" pull_request merge "$code_only" 'git rm -q README.md && git mv scheduler/executor.go README.md'
check "head is not a merge commit, pull request" pull_request plain "$all" 'echo x >>SKILL.md'
check "quote coordinator, pull request" pull_request merge "code=true quote=true egress_docker=false merge_paper=false update=false migrate=false shared_feed=false" 'echo x >>scheduler/quote_coordinator.go'
check "egress controller, pull request" pull_request merge "code=true quote=false egress_docker=true merge_paper=false update=false migrate=false shared_feed=false" 'echo x >>scheduler/cmd/egress-controller/manifest.go'
check "merge script, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true shared_feed=false" 'echo x >>scripts/merge-paper-instance.sh'
check "update script, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=false shared_feed=true" 'echo x >>scripts/update.sh'
check "layout tool, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true shared_feed=true" 'echo x >>scripts/migrate-service-layout.py'
check "status port source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true shared_feed=true" 'echo x >>scheduler/server.go'
check "subcommand dispatch, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true shared_feed=true" 'echo x >>scheduler/main.go'
check "version source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true shared_feed=true" 'echo x >>scheduler/version.go'
check "probe source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=true shared_feed=true" 'echo x >>scheduler/probe_cmd.go'
check "storage-inspect source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true shared_feed=false" 'echo x >>scheduler/storage_inspect.go'
check "lock holder source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true shared_feed=false" 'echo x >>scheduler/singleton_lock.go'
check "inspect source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true shared_feed=false" 'echo x >>scheduler/inspect_cmd.go'
check "feed health source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=false shared_feed=true" 'echo x >>scheduler/market_feed_role.go'
check "latch hook, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=false migrate=true shared_feed=false" 'echo x >>scheduler/risk.go'
check "missing-state hook, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true shared_feed=false" 'echo x >>scheduler/state_presence.go'
check "merge harness bash helper, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=false shared_feed=false" 'echo x >>scheduler/update_sh_feature_test.go'
check "gated file renamed away, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=false shared_feed=true" 'git mv scripts/update.sh scripts/moved.sh'
check "shared feed conversion, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=true shared_feed=true" 'echo x >>scripts/shared-feed-convert.sh'
check "feed implementation, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=true" 'echo x >>scheduler/market_feed_ws.go'
check "venue endpoint definition, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=true" 'echo x >>scheduler/hyperliquid_balance.go'
check "local venue helper, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=true" 'echo x >>scripts/fixtures/shared_feed_venue.py'
check "service fixture, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=true shared_feed=true" 'echo x >>scripts/test_migrate_service_layout_fixture.sh'
check "template unit, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true shared_feed=true" 'echo x >>systemd/go-trader@.service'
check "shared feed config reload, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=false shared_feed=true" 'echo x >>scheduler/config_reload.go'
check "workflow, pull request" pull_request merge "$all" 'echo x >>.github/workflows/ci.yml'

g checkout -q -B sibling "$base"
echo sibling >>"$tmp/repo/SKILL.md"
g commit -q -am sibling
sibling="$(g rev-parse HEAD)"
zeros="$(printf "%${#base}s" "" | tr ' ' 0)"
ones="$(printf "%${#base}s" "" | tr ' ' 1)"
upper="$(printf '%s' "$base" | tr a-f A-F)"

check_env "verified docs-only push" push plain "$none" base false 'echo x >>SKILL.md'
check_env "verified docs-only push, merge head" push merge "$none" base false 'echo x >>SKILL.md; echo x >>CLAUDE.md'
check_env "two docs-only commits, push" push plain "$none" base false 'echo x >>SKILL.md; g add -A; g commit -q -m d1; echo y >>README.md'
check_env "code push" push plain "$all" base false 'echo x >>scheduler/executor.go'
check_env "verified docs-only push, area rule file and generated guide" push plain "$none" base false 'echo x >>.claude/rules/backtest.md; echo x >AGENTS.md'
check_env "area rule file and generator push" push plain "$all" base false 'echo x >>.claude/rules/backtest.md; echo x >>scripts/agent_guide.py'
check_env "nested area rule path push" push plain "$all" base false 'mkdir -p .claude/rules/sub && echo x >.claude/rules/sub/x.md'
check_env "mixed push" push plain "$all" base false 'echo x >>SKILL.md; echo x >>scheduler/executor.go'
check_env "test-read doc push" push plain "$all" base false 'echo x >>docs/backtesting-registry.md'
check_env "code renamed into the list, push" push plain "$all" base false 'git rm -q README.md && git mv scheduler/executor.go README.md'
check_env "rename out of the list, push" push plain "$all" base false 'git mv README.md README2.md'
check_env "code renamed onto a new listed path, push" push plain "$all" base false 'git mv scheduler/executor.go AGENTS.md'
check_env "code then docs commits, push" push plain "$all" base false 'echo x >>scheduler/executor.go; g add -A; g commit -q -m c1; echo y >>SKILL.md'
check_env "previous commit unset, push" push plain "$all" unset false 'echo x >>SKILL.md'
check_env "previous commit empty, push" push plain "$all" "" false 'echo x >>SKILL.md'
check_env "previous commit abbreviated, push" push plain "$all" "${base:0:12}" false 'echo x >>SKILL.md'
check_env "previous commit uppercase, push" push plain "$all" "$upper" false 'echo x >>SKILL.md'
check_env "previous commit as a ref expression, push" push plain "$all" "HEAD~1" false 'echo x >>SKILL.md'
check_env "previous commit all zeros, push" push plain "$all" "$zeros" false 'echo x >>SKILL.md'
check_env "previous commit absent, push" push plain "$all" "$ones" false 'echo x >>SKILL.md'
check_env "forced flag unset, push" push plain "$all" base unset 'echo x >>SKILL.md'
check_env "forced flag empty, push" push plain "$all" base "" 'echo x >>SKILL.md'
check_env "forced push" push plain "$all" base true 'echo x >>SKILL.md'
check_env "forced flag not exactly false, push" push plain "$all" base False 'echo x >>SKILL.md'
check_env "previous commit not an ancestor, push" push plain "$all" "$sibling" false 'echo x >>SKILL.md'
check_env "empty diff, push" push plain "$all" head false 'echo x >>SKILL.md'
for cmd in diff merge-base cat-file rev-parse; do
  with_fail_git "$cmd" check_env "git $cmd fails, push" push plain "$all" base false 'echo x >>SKILL.md'
done
check_env "release with push values" release plain "$all" base false 'echo x >>SKILL.md'
check_env "manual run with push values" workflow_dispatch plain "$all" base false 'echo x >>SKILL.md'
check_env "no event with push values" "" plain "$all" base false 'echo x >>SKILL.md'
check_env "non-merge pull request with push values" pull_request plain "$all" base false 'echo x >>SKILL.md'

g checkout -q -B case "$base"
echo d1 >>"$tmp/repo/SKILL.md"
g commit -q -am d1
d1="$(g rev-parse HEAD)"
echo d2 >>"$tmp/repo/README.md"
g commit -q -am d2
git clone -q --depth 2 --branch case "file://$tmp/repo" "$tmp/shallow"
repo_dir="$tmp/shallow"
for shallow in "d1:$d1:$none" "base:$base:$all"; do
  label="${shallow%%:*}"
  rest="${shallow#*:}"
  push_before="${rest%%:*}"
  push_forced=false
  want="${rest#*:}"
  got="$(detect push)"
  if [ "$got" = "$want" ]; then
    echo "ok: shallow depth 2 clone, previous commit $label, push -> $got"
  else
    echo "FAIL: shallow depth 2 clone, previous commit $label, push: want $want, got $got"
    fail=1
  fi
done
push_before=unset
push_forced=unset
repo_dir="$tmp/repo"

svc_files=(scheduler/probe_cmd_test.go scheduler/run_evidence_test.go scheduler/market_feed_ws_test.go scheduler/egress_integration_test.go)
svc_prev=("$(res true false false false true true true)" "$(res true false false false false true false)"
  "$(res true false false false false false true)" "$(res true false true false false false true)")
for i in 0 1 2 3; do
  f="${svc_files[$i]}"
  prev="${svc_prev[$i]}"
  stem="${f%_test.go}"
  check "test body edit $f, pull request" pull_request merge "$code_only" "echo '// body edit' >>$f"
  check "test body edit $f and a doc, pull request" pull_request merge "$prev" "echo '// body edit' >>$f; echo x >>CLAUDE.md"
  check "test body edit $f and code, pull request" pull_request merge "$prev" "echo '// body edit' >>$f; echo x >>scheduler/executor.go"
  check "test file ${stem}_added_test.go added, pull request" pull_request merge "$prev" "cp $f ${stem}_added_test.go"
  check "test file $f deleted, pull request" pull_request merge "$prev" "git rm -q $f"
  check "test file $f renamed, pull request" pull_request merge "$prev" "git mv $f ${stem}_moved_test.go"
  check "test file $f made executable, pull request" pull_request merge "$prev" "chmod +x $f; echo '// body edit' >>$f"
  check "test file $f made a symlink, pull request" pull_request merge "$prev" "rm $f; ln -s executor.go $f"
  check "test file $f import changed, pull request" pull_request merge "$prev" "go_src $f 'package main' '' 'import \"io\"' '' \"\$go_body\""
  check "test file $f package clause changed, pull request" pull_request merge "$prev" "go_src $f 'package main_test' '' 'import \"os\"' '' \"\$go_body\""
  check "test file $f build constraint added, pull request" pull_request merge "$prev" "go_src $f '//go:build x' '' 'package main' '' 'import \"os\"' '' \"\$go_body\""
done
migrate_prev="${svc_prev[1]}"
probe_prev="${svc_prev[0]}"
check "two test body edits, pull request" pull_request merge "$code_only" "echo '// body edit' >>scheduler/probe_cmd_test.go; echo '// body edit' >>scheduler/run_evidence_test.go"
check "quote test body edit, pull request" pull_request merge "$(res true true false false false false false)" "echo '// body edit' >>scheduler/quote_coordinator_test.go"
check "config reload test body edit, pull request" pull_request merge "$(res true false false true false false false)" "echo '// body edit' >>scheduler/config_reload_test.go"
check "test header comment edit, pull request" pull_request merge "$migrate_prev" "go_src scheduler/run_evidence_test.go '// header comment' 'package main' '' 'import \"os\"' '' \"\$go_body\""
check "test header blank line, pull request" pull_request merge "$migrate_prev" "go_src scheduler/run_evidence_test.go 'package main' '' '' 'import \"os\"' '' \"\$go_body\""
check "test header with a block comment, pull request" pull_request merge "$migrate_prev" "echo '// body edit' >>scheduler/run_evidence_block_test.go"
check "test header with a backquote, pull request" pull_request merge "$migrate_prev" "echo '// body edit' >>scheduler/run_evidence_raw_test.go"
check "test body gains go:embed, pull request" pull_request merge "$migrate_prev" "echo '//go:embed x' >>scheduler/run_evidence_test.go"
check "test file with no top-level declaration, pull request" pull_request merge "$migrate_prev" "echo '// body edit' >>scheduler/run_evidence_nodecl_test.go"
check "test body edit under an import alias that starts with type, pull request" pull_request merge "$code_only" "echo '// body edit' >>scheduler/run_evidence_alias_test.go"
check "import alias that starts with type changed, pull request" pull_request merge "$migrate_prev" "go_src scheduler/run_evidence_alias_test.go 'package main' '' 'import (' 'typeé \"net\"' ')' '' \"\$go_body\""
check "nested egress controller test body edit, pull request" pull_request merge "$(res true false true false false false false)" "echo '// body edit' >>scheduler/cmd/egress-controller/manifest_test.go"
for cmd in --raw cat-file hash-object; do
  with_fail_git "$cmd" check "git $cmd fails on a test body edit, pull request" pull_request merge "$probe_prev" "echo '// body edit' >>scheduler/probe_cmd_test.go"
done
with_fail_git --name-only check "git --name-only fails, pull request" pull_request merge "$all" "echo '// body edit' >>scheduler/probe_cmd_test.go"
check "test body edit and the detector, pull request" pull_request merge "$all" "echo '// body edit' >>scheduler/probe_cmd_test.go; echo x >>scripts/ci_code_changed.sh"

for wf in .github/workflows/ci.yml .github/workflows/container.yml; do
  path="$root/$wf"
  n="$(grep -cxF '      code: ${{ steps.diff.outputs.code }}' "$path" || true)"
  if [ "$n" != 1 ]; then
    echo "FAIL: $wf declares the changes output 'code: \${{ steps.diff.outputs.code }}' $n times, want 1"
    fail=1
  fi
  n="$(grep -cxF '        id: diff' "$path" || true)"
  if [ "$n" != 1 ]; then
    echo "FAIL: $wf has $n steps with id diff, want 1"
    fail=1
  fi
  n="$(grep -cxF '        run: bash scripts/ci_code_changed.sh' "$path" || true)"
  if [ "$n" != 1 ]; then
    echo "FAIL: $wf runs scripts/ci_code_changed.sh $n times, want 1"
    fail=1
  fi
  want_env="$(printf '%s\n' '        id: diff' '        env:' '          EVENT_NAME: ${{ github.event_name }}' \
    '          PUSH_BEFORE: ${{ github.event.before }}' '          PUSH_FORCED: ${{ github.event.forced }}' \
    '        run: bash scripts/ci_code_changed.sh')"
  got_env="$(sed -n '/^        id: diff$/,/^        run: bash scripts\/ci_code_changed.sh$/p' "$path")"
  if [ "$got_env" != "$want_env" ]; then
    echo "FAIL: $wf detector step does not pass exactly EVENT_NAME, PUSH_BEFORE and PUSH_FORCED in its env block, got:"
    echo "$got_env"
    fail=1
  fi
  n="$(grep -cxF "    if: \${{ needs.changes.outputs.code == 'true' }}" "$path" || true)"
  if [ "$n" -lt 1 ]; then
    echo "FAIL: $wf gates no job on needs.changes.outputs.code"
    fail=1
  fi
done

path="$root/.github/workflows/container.yml"
refs="$(grep -o 'needs\.changes\.outputs\.[A-Za-z0-9_-]*' "$path" | sort -u | tr '\n' ' ')"
if [ "$refs" != "needs.changes.outputs.code " ]; then
  echo "FAIL: .github/workflows/container.yml reads changes outputs '$refs', want only needs.changes.outputs.code"
  fail=1
fi

path="$root/.github/workflows/ci.yml"
for gate in "${gates[@]}"; do
  n="$(grep -cxF "      $gate: \${{ steps.diff.outputs.$gate }}" "$path" || true)"
  if [ "$n" != 1 ]; then
    echo "FAIL: ci.yml declares the changes output '$gate: \${{ steps.diff.outputs.$gate }}' $n times, want 1"
    fail=1
  fi
  cond="if: \${{ needs.changes.outputs.$gate == 'true' }}"
  m="$(grep -cxF -e "    $cond" -e "        $cond" "$path" || true)"
  if [ "$m" -lt 1 ]; then
    echo "FAIL: ci.yml gates no job or step on needs.changes.outputs.$gate"
    fail=1
  fi
  n="$(grep -cE "needs\.changes\.outputs\.$gate([^A-Za-z0-9_-]|$)" "$path" || true)"
  if [ "$n" != "$m" ]; then
    echo "FAIL: ci.yml reads needs.changes.outputs.$gate outside the exact job or step condition '$cond'"
    fail=1
  fi
done
refs="$(grep -o 'needs\.changes\.outputs\.[A-Za-z0-9_-]*' "$path" | sort -u | tr '\n' ' ')"
want_refs="$(printf 'needs.changes.outputs.%s\n' "${outputs[@]}" | sort -u | tr '\n' ' ')"
if [ "$refs" != "$want_refs" ]; then
  echo "FAIL: ci.yml reads changes outputs '$refs', want exactly '$want_refs'"
  fail=1
fi
n="$(grep -c 'steps\.diff\.outputs\.' "$path" || true)"
if [ "$n" != "${#outputs[@]}" ]; then
  echo "FAIL: ci.yml reads steps.diff outputs $n times, want ${#outputs[@]} (one declaration per output)"
  fail=1
fi

path="$root/.github/workflows/ci.yml"
split_fail=0
split_err() {
  echo "FAIL: ci.yml $1"
  split_fail=1
  fail=1
}
feed_go_test='(^|[^A-Za-z0-9_])go[[:space:]]+(-C[[:space:]]+[^[:space:]]+[[:space:]]+)?test([[:space:]]|$)'
feed_run_prefix='        run: cd scheduler && go test '
feed_code_if="    if: \${{ needs.changes.outputs.code == 'true' }}"
feed_quote_if="    if: \${{ needs.changes.outputs.quote == 'true' }}"

if grep -q 'GOFLAGS' "$path"; then
  split_err "sets GOFLAGS, which can change the tests a go test command selects"
fi
n="$(grep -o 'TestFeedWeight[A-Za-z0-9_]*' "$path" | grep -cvx 'TestFeedWeightSharedProcesses' || true)"
if [ "$n" != 2 ]; then
  split_err "names the TestFeedWeight family $n times outside TestFeedWeightSharedProcesses, want 2 (the go -skip and the go-race -run)"
fi

feed_job_lines() {
  awk -v job="$1" '
    /^#/ { next }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
      h = $0
      sub(/^  /, "", h)
      sub(/:[[:space:]]*$/, "", h)
      injob = (h == job)
      next
    }
    /^[^ ]/ { injob = 0 }
    injob { print }
  ' "$path"
}

feed_step_lines() {
  awk -v at="$2" '
    /^      - / { step++ }
    { line[NR] = $0; of[NR] = step }
    END {
      if (of[at] == 0) { exit }
      for (i = 1; i <= NR; i++) {
        if (of[i] == of[at] && line[i] !~ /^[[:space:]]*$/) { print line[i] }
      }
    }
  ' "$1"
}

feed_check_step() {
  local job="$1" file="$2" at="$3" allow_if="$4" what="$5" name="(unnamed)" line first=1
  while IFS= read -r line; do
    if [ "$first" = 1 ]; then
      first=0
      case "$line" in
        "      - name: "?*)
          name="${line#      - name: }"
          continue
          ;;
      esac
    fi
    case "$line" in
      "$feed_run_prefix"*) continue ;;
    esac
    if [ -n "$allow_if" ] && [ "$line" = "$allow_if" ]; then
      continue
    fi
    line="${line#"${line%%[! ]*}"}"
    split_err "job $job: step '$name' carries '$line'; $what"
  done < <(feed_step_lines "$file" "$at")
  if [ "$first" = 1 ]; then
    split_err "job $job: runs a go test command outside a step"
  fi
}

feed_check_cmd() {
  local job="$1" mode="$2" args="$3" want_flag want_pkg label tok next i=0 nfilter=0 filters="" pkgs="" race=0 nocache=0 last=-1 lastpos=-2
  local -a toks
  if [ "$mode" = go ]; then
    want_flag=-skip
    want_pkg=./...
    label="the test command"
  else
    want_flag=-run
    want_pkg=.
    label="the feed weight command"
  fi
  read -r -a toks <<<"$args"
  while [ "$i" -lt "${#toks[@]}" ]; do
    tok="${toks[$i]}"
    last="$i"
    case "$tok" in
      -v | -json) ;;
      -count=1) nocache=1 ;;
      -race)
        if [ "$mode" = race ]; then
          race=1
        else
          split_err "job $job: has the flag '$tok', which the feed weight split does not allow"
        fi
        ;;
      -timeout)
        i=$((i + 1))
        next="${toks[$i]:-}"
        if ! printf '%s\n' "$next" | grep -Eqx '[0-9]+m'; then
          split_err "job $job: has the flag '$tok $next', which the feed weight split does not allow"
        fi
        ;;
      -run | -skip)
        i=$((i + 1))
        next="${toks[$i]:-}"
        if [ "$tok" = "$want_flag" ]; then
          nfilter=$((nfilter + 1))
          filters="$filters $next"
        else
          split_err "job $job: has the flag '$tok', which the feed weight split does not allow"
        fi
        ;;
      -*)
        split_err "job $job: has the flag '$tok', which the feed weight split does not allow"
        ;;
      *)
        pkgs="$pkgs $tok"
        lastpos="$i"
        ;;
    esac
    i=$((i + 1))
  done
  filters="${filters# }"
  if [ "$nfilter" != 1 ] || [ "$filters" != "'^TestFeedWeight'" ]; then
    split_err "job $job: $label must pass $want_flag '^TestFeedWeight' exactly once, found ${filters:-none}"
  fi
  if [ "$mode" = race ] && [ "$race" != 1 ]; then
    split_err "job $job: the feed weight command lacks -race"
  fi
  if [ "$mode" = race ] && [ "$nocache" != 1 ]; then
    split_err "job $job: the feed weight command lacks -count=1"
  fi
  pkgs="${pkgs# }"
  if [ "$pkgs" != "$want_pkg" ] || [ "$lastpos" != "$last" ]; then
    split_err "job $job: must name the packages '$want_pkg', found '$pkgs'"
  fi
}

feed_check_quote() {
  local args="$1" tok i=0 nrun=0 race=0 nocache=0
  local -a toks
  read -r -a toks <<<"$args"
  while [ "$i" -lt "${#toks[@]}" ]; do
    tok="${toks[$i]}"
    case "$tok" in
      -race) race=1 ;;
      -count=1) nocache=1 ;;
      -run) nrun=$((nrun + 1)) ;;
    esac
    i=$((i + 1))
  done
  if [ "$nrun" != 1 ] || [ "$race" != 1 ] || [ "$nocache" != 1 ] || [ "${toks[$((${#toks[@]} - 1))]}" != . ]; then
    split_err "job go-race-quote: the quote coordinator command must pass -race, -count=1 and -run '^TestQuote' once and end with the package '.': $args"
  fi
}

for job in go go-race go-race-quote; do
  n="$(grep -cE "^  $job:[[:space:]]*\$" "$path" || true)"
  if [ "$n" != 1 ]; then
    split_err "declares the job $job $n times, want 1"
    continue
  fi
  file="$tmp/feed-job-$job"
  feed_job_lines "$job" >"$file"
  n="$(grep -cxF '    needs: changes' "$file" || true)"
  if [ "$n" != 1 ]; then
    split_err "job $job: carries 'needs: changes' $n times at job level, want 1"
  fi
  job_if="$feed_code_if"
  if [ "$job" = go-race-quote ]; then
    job_if="$feed_quote_if"
  fi
  n="$(grep -cxF "$job_if" "$file" || true)"
  if [ "$n" != 1 ]; then
    split_err "job $job: carries the job condition '${job_if#    }' $n times, want 1"
  fi
  while IFS= read -r line; do
    case "$line" in
      "    needs: changes" | "$job_if" | "    steps:" | "    runs-on: "?* | "    timeout-minutes: 7") ;;
      *) split_err "job $job: has the job-level line '${line#    }'; only needs, the job condition, runs-on, timeout-minutes: 7 and steps are allowed" ;;
    esac
  done < <(grep -E '^    [^ ]' "$file" || true)

  n="$(grep -cE "$feed_go_test" "$file" || true)"
  if [ "$n" != 1 ]; then
    split_err "job $job: has $n go test commands, want 1"
  fi
  nfeed=0
  nquote=0
  while IFS= read -r hit; do
    at="${hit%%:*}"
    line="${hit#*:}"
    case "$line" in
      "$feed_run_prefix"[!\ ]*) ;;
      *)
        split_err "job $job: runs go test in a form this check cannot read: ${line#"${line%%[! ]*}"}"
        continue
        ;;
    esac
    args="${line#"$feed_run_prefix"}"
    case " $args " in
      *" -run '^TestQuote' "*)
        if [ "$job" = go-race-quote ]; then
          nquote=$((nquote + 1))
          feed_check_quote "$args"
          feed_check_step "$job" "$file" "$at" "" "the quote race step allows only name and run"
          continue
        fi
        ;;
    esac
    nfeed=$((nfeed + 1))
    if [ "$job" = go ]; then
      feed_check_cmd "$job" go "$args"
    else
      feed_check_cmd "$job" race "$args"
    fi
    feed_check_step "$job" "$file" "$at" "" "a feed weight test step allows only name and run"
  done < <(grep -nE "$feed_go_test" "$file" || true)
  want_feed=1
  want_quote=0
  if [ "$job" = go-race-quote ]; then
    want_feed=0
    want_quote=1
  fi
  if [ "$nfeed" != "$want_feed" ]; then
    split_err "job $job: has $nfeed feed weight split commands, want $want_feed"
  fi
  if [ "$nquote" != "$want_quote" ]; then
    split_err "job $job: has $nquote quote coordinator race commands with -run '^TestQuote', want $want_quote"
  fi
done

wiring="$root/scheduler/update_sh_syntax_test.go"
n="$(grep -cxF 'func TestShellSuiteCIWiring(t *testing.T) {' "$wiring" 2>/dev/null || true)"
b="$(grep -cE '^//go:build|^// \+build' "$wiring" 2>/dev/null || true)"
if [ "$n" != 1 ] || [ "$b" != 0 ]; then
  echo "FAIL: scheduler/update_sh_syntax_test.go must declare TestShellSuiteCIWiring once with no build constraint, so the go job runs the feed weight membership check"
  split_fail=1
  fail=1
fi
if [ "$split_fail" = 0 ]; then
  echo "ok: ci.yml go and go-race split the feed weight tests (go runs ./... with -skip '^TestFeedWeight' and no other selection flag, go-race runs -race -count=1 -run '^TestFeedWeight' . in an unconditional step, both under the code gate, and TestShellSuiteCIWiring has no build constraint)"
fi

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "PASS: ci change detector (docs-only pull request merges and verified docs-only pushes skip, with flat .claude/rules/<name>.md area rule files and the generated AGENTS.md as docs and the guide generator, its fixture script, other .claude files and nested or non-Markdown rule paths as code; code, doubtful pushes (forced, unset, malformed, zero, absent, non-ancestor, empty or failed diff, shallow gaps), no event, test-read doc, renames and non-merge heads run; each suite gate fires on its own paths (the shared feed manual proof warning, which gates no job, on conversion, feed implementation, venue endpoint, config parse and reload, template unit and fixture changes), on the workflow and the detector, and on every push that is not a verified docs-only push; test-body-only edits of top-level scheduler Go test files with the same mode and header turn the three service gates and the manual proof warning off, and every other test file change keeps them; both workflows wire the code output and pass both push values to the detector step, and ci.yml gates jobs and steps only on declared outputs)"
