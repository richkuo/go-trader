#!/usr/bin/env bash
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
lane="$root/scripts/ci_lane.sh"
tmp="$(mktemp -d)" || exit 2
trap 'rm -rf "$tmp"' EXIT
fail=0
out=""

fresh() {
  export RUNNER_TEMP="$tmp/$1"
  mkdir -p "$RUNNER_TEMP"
  date +%s >"$RUNNER_TEMP/ci-job-started"
}

bad() {
  echo "FAIL: $1"
  echo "$out"
  fail=1
}

expect() {
  local want="$1" label="$2" rc=0
  shift 2
  out="$(bash "$lane" "$@" 2>&1)" || rc=$?
  [ "$rc" = "$want" ] || bad "$label: exit $rc, want $want"
}

says() {
  case "$out" in
    *"$2"*) ;;
    *) bad "$1: the output lacks '$2'" ;;
  esac
}

fresh pass
expect 0 "start of a passing lane" start one -- bash -c 'sleep 1; echo first lane output'
expect 0 "start of a second lane" start --no-skips two -- echo '--- PASS: TestTwo (0.00s)'
expect 0 "join of two passing lanes" join
says "join of two passing lanes" "first lane output"
says "join of two passing lanes" "LANE: one exit 0"
says "join of two passing lanes" "LANE: two exit 0"
expect 1 "a second join" join
expect 1 "a lane after the join" start late -- true

fresh failing
expect 0 "start of a failing lane" start broken -- bash -c 'echo broken lane output; exit 7'
expect 0 "start of a passing lane beside it" start fine -- true
expect 1 "join with a failing lane" join
says "join with a failing lane" "broken lane output"
says "join with a failing lane" "lane broken exited 7"

fresh skips
expect 0 "start of a lane that skips" start --no-skips skipping -- echo '    --- SKIP: TestThing/part (0.00s)'
expect 1 "join with a forbidden skip" join
says "join with a forbidden skip" "lane skipping reported --- SKIP"

fresh allowed
expect 0 "start of a lane that may skip" start allowed -- echo '--- SKIP: TestThing (0.00s)'
expect 0 "join with a permitted skip" join

fresh lost
expect 0 "start of a lane that dies" start dead -- bash -c 'kill -9 "$PPID"; sleep 2'
expect 1 "join with a lane that died without a result" join
says "join with a lane that died without a result" "lane dead exited lost"

fresh missing
expect 0 "start of a command that does not exist" start missing -- /nonexistent/command
expect 1 "join with a command that does not exist" join

fresh slow
expect 0 "start of a slow lane" start slow -- sleep 20
expect 0 "start of a quick lane beside it" start quick -- echo quick lane output
bash "$lane" join >"$tmp/slow.log" 2>&1 &
pid=$!
sleep 3
kill -KILL "$pid"
wait "$pid" 2>/dev/null
out="$(cat "$tmp/slow.log")"
says "a join stopped before the slow lane ends" "quick lane output"
pkill -P "$(cat "$RUNNER_TEMP/ci-lanes/slow/pid")" 2>/dev/null

fresh deadline
export CI_LANE_LIMIT=5 CI_LANE_GRACE=3
expect 0 "start of a lane that outlives the deadline" start outlives -- bash -c 'echo partial-out; sleep 600'
expect 0 "start of a lane that ends before the deadline" start early -- echo early lane output
began="$(date +%s)"
expect 1 "join at the deadline" join
took=$(($(date +%s) - began))
[ "$took" -le 12 ] || bad "join at the deadline: ended after ${took}s, want at most 12s"
says "join at the deadline" "partial-out"
says "join at the deadline" "LANE: early exit 0"
says "join at the deadline" "lane outlives did not end before the job deadline"
says "join at the deadline" "LANE: outlives exit timeout"
pkill -P "$(cat "$RUNNER_TEMP/ci-lanes/outlives/pid")" 2>/dev/null

fresh deadline-test-binary
cat >"$tmp/held.test" <<'EOF'
import signal
import sys
import time


def dump(*_):
    print("output held until the test binary ends", flush=True)
    sys.exit(2)


signal.signal(signal.SIGQUIT, dump)
while True:
    time.sleep(1)
EOF
expect 0 "start of a lane with a test binary" start held -- bash -c 'python3 -I "$1" -test.v; exit $?' held "$tmp/held.test"
expect 1 "join that stops a test binary" join
says "join that stops a test binary" "output held until the test binary ends"
says "join that stops a test binary" "LANE: held exit timeout"

fresh before-deadline
export CI_LANE_LIMIT=30
expect 0 "start of a lane that ends in time" start intime -- bash -c 'sleep 2; echo in time'
expect 0 "join before the deadline" join
says "join before the deadline" "LANE: intime exit 0"
unset CI_LANE_LIMIT CI_LANE_GRACE

fresh no-start-time
rm "$RUNNER_TEMP/ci-job-started"
expect 2 "a lane with no job start time" start early -- true
expect 2 "a join with no job start time" join
says "a join with no job start time" "ci-job-started"

fresh ram
mkdir "$tmp/ram-dir"
export CI_LANE_RAM_DIR="$tmp/ram-dir"
expect 0 "start of a lane with a memory temp directory" start --ram-tmp --no-skips ram -- bash -c 'echo "tmp=$TMPDIR"'
expect 0 "join of a lane with a memory temp directory" join
says "a lane with a memory temp directory" "tmp=$tmp/ram-dir/ci-lane."
fresh ram-missing
export CI_LANE_RAM_DIR="$tmp/no-such-dir"
expect 2 "a lane with no memory temp directory" start --ram-tmp other -- true
unset CI_LANE_RAM_DIR

fresh empty
expect 1 "join with no lane" join
expect 0 "first start of a name" start twice -- true
expect 1 "second start of a name" start twice -- true
expect 2 "a bad lane name" start Bad_Name -- true
expect 2 "a lane with no command" start nothing --
expect 2 "a lane with no separator" start nothing true
expect 2 "no action"

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "PASS: ci lanes (every started lane is joined once; a non-zero exit, a lost lane, a missing command and a forbidden --- SKIP fail the join; each lane is printed when it ends; at the job deadline the join stops the test binary of each unfinished lane, prints its output and fails; a missing job start time, a repeated name, a start after the join and an empty join fail)"
