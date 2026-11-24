#!/usr/bin/env bash
set -uo pipefail

usage() {
    echo "usage: $0 start [--ram-tmp] [--no-skips] <name> -- <command> [args...]" >&2
    echo "       $0 join" >&2
    exit 2
}

dir="${RUNNER_TEMP:?set RUNNER_TEMP to the job temp directory}/ci-lanes"
limit="${CI_LANE_LIMIT:-395}"
grace="${CI_LANE_GRACE:-8}"

job_started() {
    local started
    started="$(cat "$RUNNER_TEMP/ci-job-started" 2>/dev/null)"
    if [[ ! "$started" =~ ^[0-9]+$ || ! "$limit" =~ ^[0-9]+$ || ! "$grace" =~ ^[0-9]+$ ]]; then
        echo "::error::the first step of a lane job must run: date +%s >\"\$RUNNER_TEMP/ci-job-started\"" >&2
        exit 2
    fi
    echo "$started"
}

start_lane() {
    local no_skips=0 ram_tmp=0 tmp name
    if [[ "${1:-}" == "--ram-tmp" ]]; then
        ram_tmp=1
        shift
    fi
    if [[ "${1:-}" == "--no-skips" ]]; then
        no_skips=1
        shift
    fi
    name="${1:-}"
    [[ "$name" =~ ^[a-z][a-z0-9-]*$ && "${2:-}" == "--" && $# -ge 3 ]] || usage
    shift 2
    job_started >/dev/null || exit 2
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
    if [[ "$ram_tmp" == 1 ]]; then
        tmp="$(mktemp -d "${CI_LANE_RAM_DIR:-/dev/shm}/ci-lane.XXXXXX")" || exit 2
        export TMPDIR="$tmp"
    fi
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

report_lane() {
    local name="$1" rc took="unknown" failed=0
    rc="$(cat "$dir/$name/rc")"
    if [[ -s "$dir/$name/ended" ]]; then
        took="$(($(cat "$dir/$name/ended") - $(cat "$dir/$name/started")))s"
    fi
    echo "::group::lane $name: exit $rc, $took"
    cat "$dir/$name/log" 2>/dev/null || true
    echo "::endgroup::"
    if [[ "$rc" != "0" ]]; then
        echo "::error::lane $name exited $rc after $took"
        failed=1
    fi
    if [[ "$(cat "$dir/$name/no-skips")" != "0" ]] && grep -n -- '--- SKIP' "$dir/$name/log"; then
        echo "::error::lane $name reported --- SKIP, so a test proved nothing"
        failed=1
    fi
    echo "LANE: $name exit $rc $took"
    return "$failed"
}

lane_test_binaries() {
    ps -A -o pid=,ppid=,args= | awk -v root="$1" '
        { pid[NR] = $1; parent[$1] = $2; $1 = ""; $2 = ""; args[NR] = $0 }
        END {
            for (i = 1; i <= NR; i++) {
                if (args[i] !~ /\.test -test\./) continue
                p = pid[i]
                for (hop = 0; hop < 64 && p != "" && p != root; hop++) p = parent[p]
                if (p == root) print pid[i]
            }
        }'
}

stop_lane() {
    local name="$1" pid pids=()
    while IFS= read -r pid; do
        [[ -n "$pid" ]] && pids+=("$pid")
    done < <(lane_test_binaries "$(cat "$dir/$name/pid" 2>/dev/null)")
    [[ ${#pids[@]} -gt 0 ]] && kill -QUIT "${pids[@]}" 2>/dev/null
}

timed_out_lanes() {
    local reported="$1" name end failed=0 pending
    shift
    for name in "$@"; do
        [[ "$reported" == *" $name "* || -e "$dir/$name/rc" ]] || stop_lane "$name"
    done
    end=$(($(date +%s) + grace))
    while :; do
        pending=0
        for name in "$@"; do
            [[ "$reported" == *" $name "* || -e "$dir/$name/rc" ]] || pending=1
        done
        [[ "$pending" == 1 && "$(date +%s)" -lt "$end" ]] || break
        sleep 1
    done
    for name in "$@"; do
        [[ "$reported" == *" $name "* ]] && continue
        if [[ "$(cat "$dir/$name/rc" 2>/dev/null)" != "0" ]]; then
            echo timeout >"$dir/$name/rc"
            echo "::error::lane $name did not end before the job deadline"
        fi
        report_lane "$name" || failed=1
    done
    return "$failed"
}

join_lanes() {
    [[ $# -eq 0 ]] || usage
    local name pid failed=0 pending reported=" " deadline
    local names=()
    deadline="$(job_started)" || exit 2
    deadline=$((deadline + limit))
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
    while :; do
        pending=0
        for name in "${names[@]}"; do
            [[ "$reported" == *" $name "* ]] && continue
            if [[ ! -e "$dir/$name/rc" ]]; then
                pid="$(cat "$dir/$name/pid" 2>/dev/null)"
                if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
                    pending=1
                    continue
                fi
                [[ -e "$dir/$name/rc" ]] || echo lost >"$dir/$name/rc"
            fi
            report_lane "$name" || failed=1
            reported+="$name "
        done
        [[ "$pending" == 1 ]] || break
        if [[ "$(date +%s)" -ge "$deadline" ]]; then
            timed_out_lanes "$reported" "${names[@]}" || failed=1
            break
        fi
        sleep 1
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
