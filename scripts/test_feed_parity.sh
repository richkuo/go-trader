#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PARITY="$SCRIPT_DIR/feed-parity.sh"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/feed-parity-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

fail() {
    echo "FAIL: $*" >&2
    if [[ -s "$WORK/out" ]]; then
        echo "--- feed-parity output:" >&2
        cat "$WORK/out" >&2
    fi
    exit 1
}

ok() {
    echo "ok: $*"
}

seal() {
    local key=$1 source=$2 instance=$3 hash=$4
    echo "[feed-seal] key=$key status=sealed source=$source instance=$instance generation=1 hash=$hash bytes=100 keys=2 ready=2 stale=0 mids=2 prepare_ms=5 sealed_at_ms=${key}000 requests=0 refused=0"
}

audit() {
    local key=$1 hash=$2 binst=$3 bhash=$4 filled=$5 unready=$6
    echo "[feed-audit] key=$key status=sealed endpoint=primary source=websocket instance=P1 generation=1 hash=$hash bytes=100 keys=2 ready=1 strategies=2 cadences=60 at=$key waited_ms=5 backup_instance=$binst backup_generation=1 backup_hash=$bhash filled=$filled unready=$unready fill_request=\"sealed\" gaps=\"none\""
}

legacy_audit() {
    local key=$1 hash=$2
    echo "[feed-audit] key=$key status=sealed endpoint=primary source=websocket instance=P1 generation=1 hash=$hash bytes=100 keys=2 ready=2 strategies=2 cadences=60 at=$key waited_ms=5 gaps=\"none\""
}

payload() {
    local key=$1 frames=$2 sha=$3
    echo "[feed-payload] key=$key kind=single owner=s-ETH frames=$frames coins=ETH sha256=$sha bytes=10"
}

{
    seal 960 websocket P1 p960
    seal 1020 websocket P1 p1020
    seal 1080 websocket P1 p1080
} >"$WORK/primary.log"
{
    seal 960 rest B1 b960
    seal 1020 rest B1 b1020
} >"$WORK/backup.log"
{
    seal 960 rest B2 c960
    seal 1020 rest B2 c1020
} >"$WORK/backup2.log"

run() {
    local a=$1 c=$2
    shift 2
    printf '%s\n' "$a" >"$WORK/a.log"
    printf '%s\n' "$c" >"$WORK/c.log"
    local rc=0
    bash "$PARITY" --feed-log "primary=$WORK/primary.log" --feed-log "backup=$WORK/backup.log" --feed-log "backup2=$WORK/backup2.log" \
        --consumer-log "live=$WORK/a.log" --consumer-log "paper=$WORK/c.log" "$@" >"$WORK/out" 2>&1 || rc=$?
    return "$rc"
}

expect_pass() {
    local label=$1
    shift
    run "$@" || fail "$label: feed-parity failed"
    grep -qx 'feed-parity: PASS' "$WORK/out" || fail "$label: no PASS line"
}

expect_fail() {
    local label=$1 needle=$2
    shift 2
    if run "$@"; then
        fail "$label: feed-parity passed"
    fi
    grep -qF -- "$needle" "$WORK/out" || fail "$label: output lacks: $needle"
}

FILLED_A=$(printf '%s\n%s\n%s' "$(audit 960 p960 B1 b960 'ETH|1h' none)" "$(payload 960 'ETH|1h:120' sha-fill)" "$(audit 1020 p1020 none none none none)")

expect_pass "fills from one backup seal" "$FILLED_A" "$FILLED_A"
grep -qF 'EXCEPTION consumer live: ETH|1h filled from backup instance B1 at 1 key(s): 960' "$WORK/out" || fail "fills are not listed as exceptions"
grep -qF '1 shared payload specs matched' "$WORK/out" || fail "the filled payload pair was not compared"
ok "consumers with fills from one backup seal pass and list the fills"

expect_fail "wrong backup hash" "backup_hash bad960 differs from feed backup hash b960" \
    "$(printf '%s\n%s' "$(audit 960 p960 B1 bad960 'ETH|1h' none)" "$(audit 1020 p1020 none none none none)")" "$FILLED_A"
ok "a backup_hash that differs from the backup feed's seal fails"

expect_fail "missing backup seal" "from backup instance B9 but no feed input logged that seal" \
    "$(printf '%s\n%s' "$(audit 960 p960 B9 z960 'ETH|1h' none)" "$(audit 1020 p1020 none none none none)")" "$FILLED_A"
ok "a backup seal that no feed input logged fails"

expect_fail "same-seal contradiction" "live filled ETH|1h and paper lists it unready from the same primary seal and the same backup seal B1/b960" \
    "$FILLED_A" "$(printf '%s\n%s' "$(audit 960 p960 B1 b960 none 'ETH|1h')" "$(audit 1020 p1020 none none none none)")"
ok "two consumers on identical seals that disagree on a key fail"

expect_pass "fill difference without a backup seal" \
    "$FILLED_A" "$(printf '%s\n%s' "$(audit 960 p960 none none none 'ETH|1h')" "$(audit 1020 p1020 none none none none)")"
grep -qF 'live filled ETH|1h from backup B1 and paper left it unready with backup none (fill difference)' "$WORK/out" || fail "the fill difference is not listed"
ok "a fill difference against a consumer with no backup seal is an exception"

expect_fail "different payloads from one fill" "consumers built different payloads from one seal" \
    "$FILLED_A" "$(printf '%s\n%s\n%s' "$(audit 960 p960 B1 b960 'ETH|1h' none)" "$(payload 960 'ETH|1h:120' sha-other)" "$(audit 1020 p1020 none none none none)")"
ok "different payloads from the same primary and backup seals fail"

expect_pass "payloads from different backup seals" \
    "$FILLED_A" "$(printf '%s\n%s\n%s' "$(audit 960 p960 B2 c960 'ETH|1h' none)" "$(payload 960 'ETH|1h:120' sha-other)" "$(audit 1020 p1020 none none none none)")"
ok "payloads filled from different backup seals are grouped apart"

LEGACY=$(printf '%s\n%s\n%s' "$(legacy_audit 960 p960)" "$(payload 960 'BTC|1h:200' sha-btc)" "$(legacy_audit 1020 p1020)")
expect_pass "records without fill fields" "$LEGACY" "$LEGACY"
ok "older audit records without fill fields read as no fill"

expect_fail "primary hash mismatch" "hash bad differs from feed primary hash p960" \
    "$(printf '%s\n%s' "$(legacy_audit 960 bad)" "$(legacy_audit 1020 p1020)")" "$LEGACY"
ok "a primary hash that differs from the feed still fails"

expect_fail "different hashes from one source" "report different hashes from the same source and instance" \
    "$(printf '%s\n%s' "$(legacy_audit 960 bad)" "$(legacy_audit 1020 p1020)")" "$LEGACY"
ok "two consumers with different hashes from one instance still fail"

expect_fail "different payloads without fills" "consumers built different payloads from one seal" \
    "$LEGACY" "$(printf '%s\n%s\n%s' "$(legacy_audit 960 p960)" "$(payload 960 'BTC|1h:200' sha-x)" "$(legacy_audit 1020 p1020)")"
ok "different payloads from one seal without fills still fail"

expect_fail "missing coverage" "1 expected key(s) missing: 1020" \
    "$(printf '%s\n%s' "$(legacy_audit 960 p960)" "$(legacy_audit 1080 p1080)")" "$LEGACY"
ok "a missing expected key still fails"

expect_fail "empty consumer input" "no [feed-audit] records (empty input)" \
    "" "$LEGACY"
ok "an empty consumer input still fails"

echo "PASS: feed-parity audit (per-key backup fills from one backup seal pass; wrong or unlogged backup seals, same-seal fill contradictions and every earlier failure rule fail)"
