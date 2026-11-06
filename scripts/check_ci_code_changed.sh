#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
detector="$root/scripts/ci_code_changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
outputs=(code quote egress_docker merge_paper update migrate)
gates=(quote egress_docker merge_paper update migrate)

g() { git -C "$tmp/repo" -c user.email=ci@example.invalid -c user.name=ci -c init.defaultBranch=main -c advice.detachedHead=false "$@"; }

git init -q "$tmp/repo"
mkdir -p "$tmp/repo/scheduler/cmd/egress-controller" "$tmp/repo/docs" "$tmp/repo/scripts" "$tmp/repo/.github/workflows"
for f in CLAUDE.md SKILL.md README.md scheduler/main.go docs/backtesting-registry.md scheduler/cmd/egress-controller/manifest.go \
  scheduler/quote_coordinator.go scripts/merge-paper-instance.sh scripts/update.sh scripts/migrate-service-layout.py \
  scheduler/server.go scheduler/risk.go scheduler/state_presence.go scheduler/update_sh_feature_test.go \
  .github/workflows/ci.yml; do
  printf 'base\n' >"$tmp/repo/$f"
done
g add -A
g commit -q -m base
base="$(g rev-parse HEAD)"

detect() {
  local event="$1" out="$tmp/output" name lines got=""
  : >"$out"
  (cd "$tmp/repo" && EVENT_NAME="$event" GITHUB_OUTPUT="$out" bash "$detector" >/dev/null)
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

all="code=true quote=true egress_docker=true merge_paper=true update=true migrate=true"
code_only="code=true quote=false egress_docker=false merge_paper=false update=false migrate=false"
none="code=false quote=false egress_docker=false merge_paper=false update=false migrate=false"

check "docs only, pull request" pull_request merge "$none" 'echo x >>CLAUDE.md; echo x >>SKILL.md; echo x >>README.md'
check "docs and code, pull request" pull_request merge "$code_only" 'echo x >>CLAUDE.md; echo x >>scheduler/main.go'
check "docs only, push" push merge "$all" 'echo x >>SKILL.md'
check "docs only, no event" "" merge "$all" 'echo x >>SKILL.md'
check "test-read doc, pull request" pull_request merge "$code_only" 'echo x >>docs/backtesting-registry.md'
check "rename out of the list, pull request" pull_request merge "$code_only" 'git mv README.md README2.md'
check "code renamed into the list, pull request" pull_request merge "$code_only" 'git rm -q README.md && git mv scheduler/main.go README.md'
check "head is not a merge commit, pull request" pull_request plain "$all" 'echo x >>SKILL.md'
check "quote coordinator, pull request" pull_request merge "code=true quote=true egress_docker=false merge_paper=false update=false migrate=false" 'echo x >>scheduler/quote_coordinator.go'
check "egress controller, pull request" pull_request merge "code=true quote=false egress_docker=true merge_paper=false update=false migrate=false" 'echo x >>scheduler/cmd/egress-controller/manifest.go'
check "merge script, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true" 'echo x >>scripts/merge-paper-instance.sh'
check "update script, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=false" 'echo x >>scripts/update.sh'
check "layout tool, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=true migrate=true" 'echo x >>scripts/migrate-service-layout.py'
check "status port source, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true" 'echo x >>scheduler/server.go'
check "latch hook, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=false migrate=true" 'echo x >>scheduler/risk.go'
check "missing-state hook, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=true" 'echo x >>scheduler/state_presence.go'
check "merge harness bash helper, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=true update=false migrate=false" 'echo x >>scheduler/update_sh_feature_test.go'
check "gated file renamed away, pull request" pull_request merge "code=true quote=false egress_docker=false merge_paper=false update=true migrate=false" 'git mv scripts/update.sh scripts/moved.sh'
check "workflow, pull request" pull_request merge "$all" 'echo x >>.github/workflows/ci.yml'

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

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "PASS: ci change detector (docs-only pull request merges skip; code, push, no event, test-read doc, renames and non-merge heads run; each suite gate fires on its own paths, on the workflow and the detector, and on every push; both workflows wire the code output and ci.yml gates jobs and steps only on declared outputs)"
