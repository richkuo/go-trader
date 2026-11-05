#!/usr/bin/env bash
set -uo pipefail

docs_only=(CLAUDE.md AGENTS.md README.md SKILL.md docs/POST_UPDATE_HISTORY.md docs/DOCKER.md)
gates=(quote egress_docker merge_paper update migrate)
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
        scheduler/go.mod scheduler/go.sum)
      ;;
    update)
      pats=(scripts/update.sh 'scripts/update_*' scripts/install-service.sh scripts/migrate-service-layout.py
        scripts/shared-feed-convert.sh 'systemd/*' go-trader.service scripts/test_migrate_service_layout_fixture.sh
        scripts/run_ci_shell_suite.sh)
      ;;
    migrate)
      pats=(scripts/migrate-service-layout.py 'scripts/update_*' scripts/install-service.sh scripts/merge-paper-instance.sh
        scripts/paper_alias.py scripts/shared-feed-convert.sh 'systemd/*' go-trader.service
        'scheduler/run_evidence*.go' scripts/test_migrate_service_layout_fixture.sh
        scripts/run_ci_shell_suite.sh)
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

out="${GITHUB_OUTPUT:-/dev/stdout}"
code=true
quote=true
egress_docker=true
merge_paper=true
update=true
migrate=true

if [ "${EVENT_NAME:-}" = pull_request ] \
  && git rev-parse -q --verify HEAD^2 >/dev/null \
  && files=$(git diff --name-only --no-renames HEAD^1 HEAD) \
  && [ -n "$files" ]; then
  code=false
  for g in "${gates[@]}"; do
    printf -v "$g" '%s' false
  done
  while IFS= read -r f; do
    hit=false
    for d in "${docs_only[@]}"; do
      if [ "$f" = "$d" ]; then
        hit=true
      fi
    done
    if [ "$hit" = false ]; then
      code=true
      echo "code change: $f"
    fi
    for g in "${gates[@]}"; do
      if in_gate "$f" "$g"; then
        printf -v "$g" '%s' true
        echo "$g change: $f"
      fi
    done
  done <<<"$files"
fi

echo "code=$code" >>"$out"
for g in "${gates[@]}"; do
  echo "$g=${!g}" >>"$out"
done
echo "code=$code quote=$quote egress_docker=$egress_docker merge_paper=$merge_paper update=$update migrate=$migrate (event ${EVENT_NAME:-unset})"
