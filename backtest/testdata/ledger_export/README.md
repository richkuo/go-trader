# Booked-ledger comparison fixture (issue 1686)

Shared by `scheduler/ledger_export_fixture_test.go` (Go CI, no Python) and
`backtest/tests/test_ledger_compare.py` (Python CI, no Go). The contract is in
SKILL.md, section Booked-Ledger Export, subsection Ledger comparison.

## Files

| Path | What it is | Produced by |
|------|------------|-------------|
| `snapshot/` | Real `go-trader export capture` output: `capture.json`, `config.json`, `state/primary.db` (rollback mode). Nothing else may be in this directory: the exporter refuses any entry the manifest does not list. | Linux capture, below |
| `export.json`, `export_scale_in.json`, `export_manual.json` | Real `go-trader export ledger` output for `hl-strict-btc`, `hl-scalein-btc` and `hl-manual-eth` (partition `live`). `inspected_revision` is `null` because the Go test binary has no `SourceCommit`. | `TestLedgerExportFixture` with the regeneration switch |
| `fixture_production.json` | The documented captures (capture-manifest sha256, `source/seed.sql` sha256, whether the seed changed, how and why) and the list of exports the Go test checks. | Edited by hand after each capture |
| `comparison_input*.json` | `go-trader.ledger-comparison-input` version 1 for each export. | Edited by hand; rebind as below |
| `market/` | Frozen synthetic BTC 1h candles, hourly funding, venue meta and `manifest.json` (`offline_candle_manifest/v1`, window `comparison` = 2026-01-05T00:00Z to 2026-01-06T20:00Z, 60 warm-up bars). Not market data. | `uv run --no-sync python backtest/testdata/ledger_export/market/generate_market.py` (deterministic, no network) |
| `source/config.json` | Source configuration (config version 20) with synthetic absolute paths under `/srv/go-trader-ledger-fixture`, outside any shared temporary root. | Hand-written |
| `source/seed.sql` | One SQL statement per line, applied to the schema the real binary creates. The `hl-strict-btc` rows come from an independent `Backtester` run over `market/` with venue-like rounding (whole-dollar prices, fees to 6 decimals, record time 41 s after the bar open); the scale-in, manual, funding, legacy-row, unresolved-row and wallet-orphan rows are hand-written. | `uv run --no-sync python backtest/testdata/ledger_export/source/build_seed.py` |
| `source/capture_fixture.sh` | Builds the binary and the pinned-driver fixture helper, creates the schema, applies the seed, converts the source to rollback mode, and runs the real `export capture`. Linux only. | |

## Regenerating the exports (exporter output changed by design)

Use this after an export schema change, a `CurrentConfigVersion` increase, or a
change to effective defaults or configuration serialization:

```bash
GO_TRADER_REGENERATE_LEDGER_EXPORT_FIXTURE=1 go -C scheduler test -run '^TestLedgerExportFixture$' -count=1 .
go -C scheduler test -run '^TestLedgerExportFixture$' -count=1 .
uv run --no-sync python -m pytest backtest/tests/test_ledger_compare.py
```

The switch runs the real export path and writes the files; CI never sets it.
Without an export schema-version change it refuses to write when any section
other than `current_effective_configuration` changes, except that a documented
re-capture may also change `capture_manifest_sha256`, `capture` and
`snapshot_files`, and a re-capture whose `fixture_production.json` entry records
`source_data_changed: true` may also change `events` and `wallet_orphan_context`.
Commit the regenerated files and show their diff in the pull request. If
`events` or `wallet_orphan_context` changed, rebind each comparison input:

```bash
uv run --no-sync python - <<'EOF'
import json, sys
sys.path.insert(0, "backtest")
import ledger_compare as lc
d = "backtest/testdata/ledger_export/"
for exp, inp in (("export.json", "comparison_input.json"),
                 ("export_scale_in.json", "comparison_input_scale_in.json"),
                 ("export_manual.json", "comparison_input_manual.json")):
    cin = json.load(open(d + inp))
    cin["export"]["booked_sections_sha256"] = lc.booked_sections_sha256(json.load(open(d + exp)))
    open(d + inp, "w").write(json.dumps(cin, indent=2) + "\n")
EOF
```

## Re-capturing the snapshot (Linux)

Re-capture when the source data changes on purpose, or when
`MinSupportedConfigVersion` rises above the snapshot's configuration version (the
exporter then refuses the committed snapshot).

1. Change `source/config.json`, `market/` or `build_seed.py` as needed and rebuild `source/seed.sql`.
2. Capture on Linux with a private mount namespace. On macOS, Docker Desktop works:

   ```bash
   mkdir -p /tmp/ledger-capture
   docker run --rm --cap-add SYS_ADMIN \
     -v "$PWD":/repo:ro -v "$(go env GOMODCACHE)":/go/pkg/mod:ro -v /tmp/ledger-capture:/out \
     -e GOFLAGS=-mod=readonly -e GOPROXY=off -e GOCACHE=/tmp/gocache -e GOTOOLCHAIN=local \
     golang:1.26.2 bash /repo/backtest/testdata/ledger_export/source/capture_fixture.sh /repo /out/snapshot
   ```

3. Replace `snapshot/` with `/tmp/ledger-capture/snapshot` (only the three listed files).
4. Append an entry to `fixture_production.json` `captures` with the new `capture.json` sha256, the `seed.sql` sha256, `source_data_changed` (true only if the seed sha256 differs from the previous entry; the Go test checks this), `captured_with` and `note`.
5. Run the regeneration commands above, then rebind the comparison inputs.

## Initial capture

Docker Desktop 29.8.1, image `FROM golang:1.26.2` plus apt `strace util-linux
procps sqlite3 python3` (Debian 13, linux/arm64, kernel `7.0.14-linuxkit`), root
with `--cap-add SYS_ADMIN`, `modernc.org/sqlite v1.57.0`:
`capture.json` sha256 `a35623c14d5675b735b9a7ba01f0cd574c0b06bf280ea67ed8acf2a437cf0703`,
`config.json` `b31b7c55a206cc7e0919b36d08e8891d828254fb4d4d5f7b3bdb4b32441131b7`,
`state/primary.db` `7f98bf61a0b8057917eb62304cfd6b09c6435ba5a2f10eafef6e26176190daa5`,
`seed.sql` `ccb78635277a2738e3de6aa3262535a9af724bae2b1bbf1f700bbd2172c22e45`.
Running the documented command above with the plain `golang:1.26.2` image
reproduced `config.json` and `state/primary.db` byte for byte; `capture.json`
differed only in its four capture times.

## What the fixtures prove

- `export.json` + `comparison_input.json`: strict success. Two positions match (one with a partial and a full take-profit close, one open at the interval end with its later closes outside the interval), strategy funding matches as an unallocated total, a pre-interval position and the post-interval closes stay listed as outside the interval, and the synthetic terminal liquidation is excluded.
- `export_scale_in.json`: strict refusal (scale-in, active stop). Booked conservation holds across an open, a scale-in, a gross partial close, a legacy full close, a legacy opening row, strategy funding and one row with no position id (unresolved). Approximate mode simulates with flat costs and is incomplete.
- `export_manual.json`: strict refusal; every event is not comparable.
