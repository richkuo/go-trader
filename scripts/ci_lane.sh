#!/usr/bin/env bash
set -uo pipefail

usage() {
    echo "usage: $0 start [--no-skips] <name> -- <command> [args...]" >&2
    echo "       $0 join" >&2
    exit 2
}

dir="${RUNNER_TEMP:?set RUNNER_TEMP to the job temp directory}/ci-lanes"

start_lane() {
    local no_skips=0 name
    if [[ "${1:-}" == "--no-skips" ]]; then
        no_skips=1
        shift
    fi
    name="${1:-}"
    [[ "$name" =~ ^[a-z][a-z0-9-]*$ && "${2:-}" == "--" && $# -ge 3 ]] || usage
    shift 2
    mkdir -p "$dir" || exit 2
    if [[ -e "$dir/joined" ]]; then
        echo "::error::lane $name starts after the join, so its result would be lost"
        exit 1
    fi
    if ! mkdir "$dir/$name"; then
        echo "::error::lane $name starts twice"
        exit 1
    fi
    echo "$no_skips" >"$dir/$name/no-skips" || exit 2
    date +%s >"$dir/$name/started" || exit 2
    nohup bash -c '
        lane=$1
        shift
        rc=0
        "$@" >"$lane/log" 2>&1 || rc=$?
        date +%s >"$lane/ended"
        echo "$rc" >"$lane/rc.tmp" && mv "$lane/rc.tmp" "$lane/rc"
    ' lane "$dir/$name" "$@" </dev/null >/dev/null 2>&1 &
    echo "$!" >"$dir/$name/pid" || exit 2
    echo "lane $name started (pid $!)"
}

lane_names() {
    local d
    for d in "$dir"/*/; do
        [[ -d "$d" ]] && basename "$d"
    done
}

print_lane() {
    local name="$1" title="$2"
    echo "::group::lane $name: $title"
    cat "$dir/$name/log" 2>/dev/null || true
    echo "::endgroup::"
}

join_lanes() {
    [[ $# -eq 0 ]] || usage
    local name rc pid took failed=0 pending
    local names=()
    while IFS= read -r name; do
        names+=("$name")
    done < <(lane_names)
    if [[ ${#names[@]} -eq 0 ]]; then
        echo "::error::no lane started, so there is nothing to join"
        exit 1
    fi
    if ! (set -o noclobber && : >"$dir/joined") 2>/dev/null; then
        echo "::error::the lanes were joined before"
        exit 1
    fi
    trap 'for name in "${names[@]}"; do [[ -e "$dir/$name/rc" ]] || print_lane "$name" "stopped before it ended"; done; exit 143' INT TERM
    while :; do
        pending=0
        for name in "${names[@]}"; do
            [[ -e "$dir/$name/rc" ]] && continue
            pid="$(cat "$dir/$name/pid" 2>/dev/null)"
            if [[ -z "$pid" ]] || ! kill -0 "$pid" 2>/dev/null; then
                [[ -e "$dir/$name/rc" ]] || echo lost >"$dir/$name/rc"
                continue
            fi
            pending=1
        done
        [[ "$pending" == 1 ]] || break
        sleep 1
    done
    trap - INT TERM
    for name in "${names[@]}"; do
        rc="$(cat "$dir/$name/rc")"
        took="unknown"
        if [[ -s "$dir/$name/ended" ]]; then
            took="$(($(cat "$dir/$name/ended") - $(cat "$dir/$name/started")))s"
        fi
        print_lane "$name" "exit $rc, $took"
        if [[ "$rc" != "0" ]]; then
            echo "::error::lane $name exited $rc after $took"
            failed=1
        fi
        if [[ "$(cat "$dir/$name/no-skips")" != "0" ]] && grep -n -- '--- SKIP' "$dir/$name/log"; then
            echo "::error::lane $name reported --- SKIP, so a test proved nothing"
            failed=1
        fi
        echo "LANE: $name exit $rc $took"
    done
    exit "$failed"
}

action="${1:-}"
[[ $# -gt 0 ]] && shift
case "$action" in
    start) start_lane "$@" ;;
    join) join_lanes "$@" ;;
    *) usage ;;
esac
