#!/usr/bin/env bash
set -uo pipefail

docs_only=(CLAUDE.md AGENTS.md README.md SKILL.md docs/POST_UPDATE_HISTORY.md docs/DOCKER.md)
out="${GITHUB_OUTPUT:-/dev/stdout}"
code=true

if [ "${EVENT_NAME:-}" = pull_request ] \
  && git rev-parse -q --verify HEAD^2 >/dev/null \
  && files=$(git diff --name-only --no-renames HEAD^1 HEAD) \
  && [ -n "$files" ]; then
  code=false
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
  done <<<"$files"
fi

echo "code=$code" >>"$out"
echo "code=$code (event ${EVENT_NAME:-unset})"
