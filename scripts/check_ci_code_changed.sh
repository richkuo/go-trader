#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
detector="$root/scripts/ci_code_changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
outputs=(code quote egress_docker merge_paper update migrate shared_feed)
gates=(quote egress_docker merge_paper update migrate shared_feed)

g() { git -C "$tmp/repo" -c user.email=ci@example.invalid -c user.name=ci -c init.defaultBranch=main -c advice.detachedHead=false "$@"; }

git init -q "$tmp/repo"
mkdir -p "$tmp/repo/scheduler/cmd/egress-controller" "$tmp/repo/docs" "$tmp/repo/scripts/fixtures" "$tmp/repo/systemd" "$tmp/repo/.github/workflows"
for f in CLAUDE.md SKILL.md README.md scheduler/executor.go docs/backtesting-registry.md scheduler/cmd/egress-controller/manifest.go \
  scheduler/quote_coordinator.go scripts/merge-paper-instance.sh scripts/update.sh scripts/migrate-service-layout.py \
  scheduler/server.go scheduler/risk.go scheduler/state_presence.go scheduler/update_sh_feature_test.go \
  scheduler/main.go scheduler/probe_cmd.go scheduler/storage_inspect.go scheduler/singleton_lock.go scheduler/inspect_cmd.go \
  scheduler/market_feed_role.go scheduler/version.go .github/workflows/ci.yml scripts/shared-feed-convert.sh \
  scheduler/market_feed_ws.go scheduler/hyperliquid_balance.go scripts/fixtures/shared_feed_venue.py \
  scripts/test_migrate_service_layout_fixture.sh systemd/go-trader@.service scheduler/config_reload.go; do
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

all="code=true quote=true egress_docker=true merge_paper=true update=true migrate=true shared_feed=true"
code_only="code=true quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=false"
none="code=false quote=false egress_docker=false merge_paper=false update=false migrate=false shared_feed=false"

check "docs only, pull request" pull_request merge "$none" 'echo x >>CLAUDE.md; echo x >>SKILL.md; echo x >>README.md'
check "docs and code, pull request" pull_request merge "$code_only" 'echo x >>CLAUDE.md; echo x >>scheduler/executor.go'
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
feed_quote_if="        if: \${{ needs.changes.outputs.quote == 'true' }}"

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
    split_err "job go-race: the quote coordinator command must pass -race, -count=1 and -run '^TestQuote' once and end with the package '.': $args"
  fi
}

for job in go go-race; do
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
  n="$(grep -cxF "$feed_code_if" "$file" || true)"
  if [ "$n" != 1 ]; then
    split_err "job $job: carries the job condition 'if: \${{ needs.changes.outputs.code == 'true' }}' $n times, want 1"
  fi
  while IFS= read -r line; do
    case "$line" in
      "    needs: changes" | "$feed_code_if" | "    steps:" | "    runs-on: "?*) ;;
      *) split_err "job $job: has the job-level line '${line#    }'; only needs, the code condition, runs-on and steps are allowed" ;;
    esac
  done < <(grep -E '^    [^ ]' "$file" || true)

  want=1
  if [ "$job" = go-race ]; then
    want=2
  fi
  n="$(grep -cE "$feed_go_test" "$file" || true)"
  if [ "$n" != "$want" ]; then
    split_err "job $job: has $n go test commands, want $want"
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
        if [ "$job" = go-race ]; then
          nquote=$((nquote + 1))
          feed_check_quote "$args"
          feed_check_step "$job" "$file" "$at" "$feed_quote_if" "the quote race step allows only name, the quote condition and run"
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
  if [ "$nfeed" != 1 ]; then
    split_err "job $job: has $nfeed feed weight split commands, want 1"
  fi
  if [ "$job" = go-race ] && [ "$nquote" != 1 ]; then
    split_err "job go-race: has $nquote quote coordinator race commands with -run '^TestQuote', want 1"
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
echo "PASS: ci change detector (docs-only pull request merges skip; code, push, no event, test-read doc, renames and non-merge heads run; each suite gate fires on its own paths (the shared feed root proof on conversion, feed implementation, venue endpoint, config parse and reload, template unit and fixture changes), on the workflow and the detector, and on every push; both workflows wire the code output and ci.yml gates jobs and steps only on declared outputs)"
