#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
    echo "capture_fixture.sh: capture needs Linux private mount namespaces; see backtest/testdata/ledger_export/README.md" >&2
    exit 1
fi
if [[ $# -ne 2 ]]; then
    echo "usage: capture_fixture.sh <repo-root> <new-output-dir>" >&2
    exit 2
fi

REPO=$(cd "$1" && pwd)
OUT=$2
ROOT=/srv/go-trader-ledger-fixture
SRC="$REPO/backtest/testdata/ledger_export/source"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[[ ! -e "$OUT" ]] || { echo "output $OUT already exists" >&2; exit 1; }
[[ ! -e "$ROOT" ]] || { echo "$ROOT already exists; use a fresh environment" >&2; exit 1; }

go -C "$REPO/scheduler" build -buildvcs=false -o "$WORK/go-trader" .
go -C "$REPO/scheduler" build -buildvcs=false -o "$WORK/ledger_fixture" ../scripts/fixtures/ledger_fixture.go
PINNED=$(awk '$1 == "modernc.org/sqlite" {print $2; exit}' "$REPO/scheduler/go.mod")
[[ "$("$WORK/ledger_fixture" version)" == "modernc.org/sqlite $PINNED" ]] || { echo "fixture helper driver differs from scheduler/go.mod" >&2; exit 1; }

mkdir -p "$ROOT/cfg" "$ROOT/state"
cp "$SRC/config.json" "$ROOT/cfg/config.json"
rc=0
(cd "$ROOT" && env -i "PATH=/usr/bin:/bin" "HOME=$WORK" HYPERLIQUID_SECRET_KEY=fixture-schema-only \
    "$WORK/go-trader" export tradingview --config "$ROOT/cfg/config.json" --all --output "$WORK/schema.csv") \
    >"$WORK/schema.out" 2>"$WORK/schema.err" || rc=$?
grep -q "no trade data found" "$WORK/schema.err" || { cat "$WORK/schema.err" >&2; echo "schema creation failed (rc $rc)" >&2; exit 1; }

mapfile -t STATEMENTS < "$SRC/seed.sql"
"$WORK/ledger_fixture" exec "$ROOT/state/primary.db" "${STATEMENTS[@]}" "PRAGMA journal_mode=DELETE"

env -i "PATH=/usr/bin:/bin" "HOME=$WORK" "$WORK/go-trader" export capture --config "$ROOT/cfg/config.json" --output-dir "$OUT"

echo "== host: $(uname -a)"
echo "== user: $(id)"
echo "== go: $(go version)"
echo "== fixture helper: $("$WORK/ledger_fixture" version)"
echo "== seed.sql sha256: $(sha256sum "$SRC/seed.sql" | cut -d' ' -f1)"
(cd "$OUT" && find . -type f | sort | xargs sha256sum)
