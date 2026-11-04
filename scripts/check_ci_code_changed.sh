#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
detector="$root/scripts/ci_code_changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

g() { git -C "$tmp/repo" -c user.email=ci@example.invalid -c user.name=ci -c init.defaultBranch=main -c advice.detachedHead=false "$@"; }

git init -q "$tmp/repo"
mkdir -p "$tmp/repo/scheduler" "$tmp/repo/docs"
for f in CLAUDE.md SKILL.md README.md scheduler/main.go docs/backtesting-registry.md; do
  printf 'base\n' >"$tmp/repo/$f"
done
g add -A
g commit -q -m base
base="$(g rev-parse HEAD)"

detect() {
  local event="$1" out="$tmp/output"
  : >"$out"
  (cd "$tmp/repo" && EVENT_NAME="$event" GITHUB_OUTPUT="$out" bash "$detector" >/dev/null)
  local lines
  lines="$(grep -c '^code=' "$out" || true)"
  if [ "$lines" != 1 ]; then
    echo "bad: $lines code= lines" && return
  fi
  sed -n 's/^code=//p' "$out"
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
    echo "ok: $name -> code=$got"
  else
    echo "FAIL: $name: want code=$want, got $got"
    fail=1
  fi
}

check "docs only, pull request" pull_request merge false 'echo x >>CLAUDE.md; echo x >>SKILL.md; echo x >>README.md'
check "docs and code, pull request" pull_request merge true 'echo x >>CLAUDE.md; echo x >>scheduler/main.go'
check "docs only, push" push merge true 'echo x >>SKILL.md'
check "docs only, no event" "" merge true 'echo x >>SKILL.md'
check "test-read doc, pull request" pull_request merge true 'echo x >>docs/backtesting-registry.md'
check "rename out of the list, pull request" pull_request merge true 'git mv README.md README2.md'
check "code renamed into the list, pull request" pull_request merge true 'git rm -q README.md && git mv scheduler/main.go README.md'
check "head is not a merge commit, pull request" pull_request plain true 'echo x >>SKILL.md'

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
  refs="$(grep -o 'needs\.changes\.outputs\.[A-Za-z0-9_-]*' "$path" | sort -u | tr '\n' ' ')"
  if [ "$refs" != "needs.changes.outputs.code " ]; then
    echo "FAIL: $wf reads changes outputs '$refs', want only needs.changes.outputs.code"
    fail=1
  fi
  n="$(grep -cxF "    if: \${{ needs.changes.outputs.code == 'true' }}" "$path" || true)"
  if [ "$n" -lt 1 ]; then
    echo "FAIL: $wf gates no job on needs.changes.outputs.code"
    fail=1
  fi
done

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "PASS: ci change detector (docs-only pull request merges skip; code, push, no event, test-read doc, renames and non-merge heads run; both workflows wire the code output)"
