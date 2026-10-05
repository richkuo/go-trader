#!/usr/bin/env bash
set -uo pipefail

usage() {
    echo "usage: $0 --marker <exact success line> [--forbid <omission text>]... -- <command> [args...]" >&2
    exit 2
}

marker=""
forbid=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --marker)
            [[ $# -ge 2 && -n "$2" ]] || usage
            marker=$2
            shift 2
            ;;
        --forbid)
            [[ $# -ge 2 && -n "$2" ]] || usage
            forbid+=("$2")
            shift 2
            ;;
        --)
            shift
            break
            ;;
        *)
            usage
            ;;
    esac
done
[[ -n "$marker" && $# -gt 0 ]] || usage

log=$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/shell-suite.XXXXXX") || exit 2
trap 'rm -f "$log"' EXIT

rc=0
"$@" 2>&1 | tee "$log" || rc=$?

failed=0
if [[ "$rc" != "0" ]]; then
    echo "::error::the suite exited $rc"
    failed=1
fi
if ! grep -Fxq -- "$marker" "$log"; then
    echo "::error::the suite did not print its success line: $marker"
    failed=1
fi
if grep -n '^SKIP:' "$log"; then
    echo "::error::the suite printed a SKIP line, so it did not prove its criteria"
    failed=1
fi
for text in ${forbid[@]+"${forbid[@]}"}; do
    if grep -nF -- "$text" "$log"; then
        echo "::error::the suite omitted a check that is not on its permitted list: $text"
        failed=1
    fi
done
exit "$failed"
