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

fresh stopped
expect 0 "start of a slow lane" start slow -- bash -c 'echo slow lane output; sleep 20'
bash "$lane" join >"$tmp/stopped.log" 2>&1 &
pid=$!
sleep 2
kill -TERM "$pid"
rc=0
wait "$pid" || rc=$?
out="$(cat "$tmp/stopped.log")"
[ "$rc" = 143 ] || bad "a stopped join: exit $rc, want 143"
says "a stopped join" "slow lane output"
pkill -P "$(cat "$RUNNER_TEMP/ci-lanes/slow/pid")" 2>/dev/null

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
echo "PASS: ci lanes (every started lane is joined once; a non-zero exit, a lost lane, a missing command and a forbidden --- SKIP fail the join; a stopped join prints the output of unfinished lanes; a repeated name, a start after the join and an empty join fail)"
