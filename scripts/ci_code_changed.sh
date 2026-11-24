#!/usr/bin/env bash
set -uo pipefail

docs_only=(CLAUDE.md AGENTS.md README.md SKILL.md docs/POST_UPDATE_HISTORY.md docs/DOCKER.md)
gates=(quote egress_docker merge_paper update migrate)
manual_gates=(shared_feed)
every_gate=(.github/workflows/ci.yml scripts/ci_code_changed.sh scripts/check_ci_code_changed.sh)

gate_patterns() {
  case "$1" in
    quote)
      pats=('scheduler/*quote*.go' scheduler/go.mod scheduler/go.sum)
      ;;
    egress_docker)
      pats=('scheduler/cmd/egress-controller/*' 'scheduler/cmd/egress-fixture/*' 'scheduler/internal/egresspolicy/*'
        'scheduler/internal/notifybroker/*' 'scheduler/egress*.go' 'scheduler/notify_broker*.go'
        'scheduler/container_supervisor*.go' 'scheduler/runtime_policy*.go' scheduler/go.mod scheduler/go.sum
        'docker/*' Dockerfile .dockerignore 'shared_tools/egress_*' pyproject.toml uv.lock .python-version
        scripts/test_egress_docker.sh scripts/run_ci_shell_suite.sh)
      ;;
    merge_paper)
      pats=(scripts/merge-paper-instance.sh scripts/paper_alias.py scripts/migrate-service-layout.py scripts/update_helpers.sh
        scripts/test_merge_paper_instance.sh 'systemd/*' scheduler/merge_paper_instance_test.go
        'scheduler/config*.go' 'scheduler/*db*.go' 'scheduler/state*.go' 'scheduler/storage*.go' 'scheduler/inspect*.go'
        'scheduler/risk_partition*.go' 'scheduler/singleton_lock*.go' 'scheduler/manual_action_lock*.go'
        scheduler/main.go scheduler/version.go scheduler/server.go scheduler/update_sh_feature_test.go
        scheduler/go.mod scheduler/go.sum)
      ;;
    update)
      pats=(scripts/update.sh 'scripts/update_*' scripts/install-service.sh scripts/migrate-service-layout.py
        scripts/shared-feed-convert.sh 'systemd/*' go-trader.service scripts/test_migrate_service_layout_fixture.sh
        scheduler/main.go scheduler/version.go 'scheduler/probe*.go' scheduler/server.go 'scheduler/market_feed_role*.go'
        scripts/run_ci_shell_suite.sh)
      ;;
    migrate)
      pats=(scripts/migrate-service-layout.py 'scripts/update_*' scripts/install-service.sh scripts/merge-paper-instance.sh
        scripts/paper_alias.py scripts/shared-feed-convert.sh 'systemd/*' go-trader.service
        'scheduler/run_evidence*.go' scheduler/risk.go scheduler/server.go scheduler/state_presence.go
        scheduler/main.go scheduler/version.go 'scheduler/probe*.go' 'scheduler/storage*.go' 'scheduler/singleton_lock*.go' 'scheduler/inspect*.go'
        scripts/test_migrate_service_layout_fixture.sh
        scripts/run_ci_shell_suite.sh)
      ;;
    shared_feed)
      pats=(scripts/shared-feed-convert.sh scripts/feed-parity.sh scripts/feed-source-compare.sh 'scripts/fixtures/shared_feed_*'
        scripts/test_migrate_service_layout_fixture.sh scripts/migrate-service-layout.py scripts/update.sh 'scripts/update_*'
        scripts/install-service.sh 'systemd/*' go-trader.service scripts/run_ci_shell_suite.sh
        'scheduler/market_feed*.go' scheduler/hyperliquid_balance.go scheduler/hyperliquid_candles.go
        scheduler/hyperliquid_funding.go scheduler/hyperliquid_marks.go scheduler/hyperliquid_lot_metadata.go 'scheduler/egress*.go' scheduler/main.go scheduler/version.go
        'scheduler/probe*.go' scheduler/server.go 'scheduler/config*.go' scheduler/go.mod scheduler/go.sum)
      ;;
    *)
      pats=()
      ;;
  esac
}

in_gate() {
  local f="$1" p
  gate_patterns "$2"
  for p in "${every_gate[@]}" "${pats[@]}"; do
    if [[ "$f" == $p ]]; then
      return 0
    fi
  done
  return 1
}

on_docs_list() {
  local d
  for d in "${docs_only[@]}"; do
    if [ "$1" = "$d" ]; then
      return 0
    fi
  done
  [[ "$1" =~ ^\.claude/rules/[a-z0-9]+(-[a-z0-9]+)*\.md$ ]]
}

scratch="$(mktemp 2>/dev/null)" || scratch=""
trap '[ -z "$scratch" ] || rm -f "$scratch"' EXIT

push_docs_only() {
  local head before f n=0
  [ -n "$scratch" ] || return 1
  [ "${PUSH_FORCED-}" = false ] || return 1
  head="$(git rev-parse -q --verify 'HEAD^{commit}')" || return 1
  [[ "$head" =~ ^[0-9a-f]+$ ]] || return 1
  before="${PUSH_BEFORE-}"
  [[ "$before" =~ ^[0-9a-f]+$ ]] || return 1
  [ "${#before}" = "${#head}" ] || return 1
  if [[ "$before" =~ ^0+$ ]]; then
    return 1
  fi
  git cat-file -e "$before^{commit}" 2>/dev/null || return 1
  git merge-base --is-ancestor "$before" "$head" || return 1
  git diff --name-only --no-renames -z "$before" "$head" >"$scratch" || return 1
  while IFS= read -r -d '' f || [ -n "$f" ]; do
    if ! on_docs_list "$f"; then
      echo "push change off the docs-only list: $f"
      return 1
    fi
    n=$((n + 1))
    f=""
  done <"$scratch"
  [ "$n" -gt 0 ]
}

go_header() {
  LC_ALL=C awk '
    !found && /^(func|var|const|type)([ \t(]|$)/ { found = 1 }
    !found {
      if (index($0, "/*") || index($0, "`")) bad = 1
      print
      next
    }
    index($0, "go:embed") { bad = 1 }
    END { if (!found || bad) exit 3 }
  '
}

test_only_edit() {
  local meta f old_mode new_mode old_id new_id old_h new_h n=0
  [ -n "$scratch" ] || return 1
  git diff --raw --no-abbrev --no-renames -z HEAD^1 HEAD >"$scratch" || return 1
  while IFS= read -r -d '' meta || [ -n "$meta" ]; do
    f=""
    IFS= read -r -d '' f || [ -n "$f" ] || return 1
    [[ "$meta" =~ ^:([0-7]+)\ ([0-7]+)\ ([0-9a-f]+)\ ([0-9a-f]+)\ M$ ]] || return 1
    old_mode="${BASH_REMATCH[1]}"
    new_mode="${BASH_REMATCH[2]}"
    old_id="${BASH_REMATCH[3]}"
    new_id="${BASH_REMATCH[4]}"
    [ "$old_mode" = "$new_mode" ] || return 1
    [ "$old_mode" = 100644 ] || [ "$old_mode" = 100755 ] || return 1
    [ "$old_id" != "$new_id" ] || return 1
    [[ "$f" =~ ^scheduler/[A-Za-z0-9_.-]+_test\.go$ ]] || return 1
    old_h="$(git cat-file blob "$old_id" </dev/null | go_header | git hash-object --stdin)" || return 1
    new_h="$(git cat-file blob "$new_id" </dev/null | go_header | git hash-object --stdin)" || return 1
    [ -n "$old_h" ] && [ "$old_h" = "$new_h" ] || return 1
    n=$((n + 1))
    meta=""
  done <"$scratch"
  [ "$n" -gt 0 ]
}

out="${GITHUB_OUTPUT:-/dev/stdout}"
code=true
quote=true
egress_docker=true
merge_paper=true
update=true
migrate=true
shared_feed=true

if [ "${EVENT_NAME:-}" = pull_request ] \
  && git rev-parse -q --verify HEAD^2 >/dev/null \
  && files=$(git diff --name-only --no-renames HEAD^1 HEAD) \
  && [ -n "$files" ]; then
  code=false
  for g in "${gates[@]}" "${manual_gates[@]}"; do
    printf -v "$g" '%s' false
  done
  while IFS= read -r f; do
    if ! on_docs_list "$f"; then
      code=true
      echo "code change: $f"
    fi
    for g in "${gates[@]}" "${manual_gates[@]}"; do
      if in_gate "$f" "$g"; then
        printf -v "$g" '%s' true
        echo "$g change: $f"
      fi
    done
  done <<<"$files"
  if test_only_edit; then
    update=false
    migrate=false
    shared_feed=false
    egress_docker=false
    echo "service gates off: every change is a test-body edit of an existing top-level scheduler Go test file with the same mode and the same package clause, build constraints and imports"
  fi
elif [ "${EVENT_NAME:-}" = push ] && push_docs_only; then
  code=false
  for g in "${gates[@]}" "${manual_gates[@]}"; do
    printf -v "$g" '%s' false
  done
  echo "verified docs-only push: every path from PUSH_BEFORE to HEAD is on the docs-only list"
fi

echo "code=$code" >>"$out"
for g in "${gates[@]}"; do
  echo "$g=${!g}" >>"$out"
done
if [ "${EVENT_NAME:-}" = pull_request ] && [ "$shared_feed" = true ]; then
  echo "::warning title=Manual proof needed::this change touches shared-feed conversion paths; no CI job runs the sharedfeed_migrated and sharedfeed_legacy scenarios, so run both by hand on a disposable Linux systemd host (SKILL.md, Root proof on real units)"
fi
echo "code=$code quote=$quote egress_docker=$egress_docker merge_paper=$merge_paper update=$update migrate=$migrate shared_feed=$shared_feed (event ${EVENT_NAME:-unset})"
