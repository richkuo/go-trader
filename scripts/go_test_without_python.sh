#!/usr/bin/env bash
set -euo pipefail

if [[ $# -eq 0 ]]; then
    echo "usage: go test -exec $0 ..." >&2
    exit 2
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/go-no-python.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin" "$WORK/.venv/bin" "$WORK/cwd/.venv/bin"
for dir in "$WORK/bin" "$WORK/.venv/bin" "$WORK/cwd/.venv/bin"; do
    for name in python python3 uv uvx; do
        printf '#!/bin/sh\necho "%s $*" >>"%s"\nexit 127\n' "$name" "$WORK/started" >"$dir/$name"
        chmod +x "$dir/$name"
    done
done

cd "$WORK/cwd"
rc=0
PATH="$WORK/bin:$PATH" "$@" || rc=$?
if [[ -s "$WORK/started" ]]; then
    echo "FAIL: a Go test that must need no Python environment started:" >&2
    cat "$WORK/started" >&2
    exit 1
fi
exit "$rc"
