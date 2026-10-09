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
| `export_paper.json`, `comparison_input_paper.json` | Synthetic paper-partition export (issue 1726): the `export.json` envelope relabeled to partition `paper` and role `paper`, every row stamped `cost_model_version` 1 with `fee_source` `modeled`, rows after the interval dropped, and the in-interval fills and funding rebooked with the version 1 paper cost model (taker fee, adverse `SlippagePct`, no spread) at the simulated bar prices. Its comparison input has `--mode=paper` args and a relative price tolerance of `1e-9`. Not a capture. | `uv run --no-sync python backtest/testdata/ledger_export/source/build_paper_fixture.py` (deterministic, no network; it refuses to write unless the result reaches strict success) |
| `export_paper_resting.json`, `comparison_input_paper_resting.json` | Synthetic paper fixture (issue 1727): the pre-interval rows of `export_paper.json`, unchanged, plus in-interval rows rebuilt from a rule-on simulation over `resting_market/` (fresh row ids ordered by time; opens recorded at bar open plus 41 s, the partial tier close at its bar open plus one bar plus 41 s with `close_reason` unavailable as the exporter writes a partial close, funding at its accrual bar time). The version 4 input uses a `tiered_tp_atr` ladder whose first tier rounds to the limit 62125 and `resting_tp_trade_through: true`. Not a capture. | `uv run --no-sync python backtest/testdata/ledger_export/source/build_paper_resting_fixture.py` (deterministic, no network; it refuses to write unless the pair reaches strict success with `resting_tier_closes_verified` and one trade-through tier close in the 2026-01-05T15:00Z bar, none in the touch bar, and the same simulated opens as a `market/` run) |
| `export_paper_resting_touch.json`, `comparison_input_paper_resting_touch.json` | The same rows with only the tier close moved to the touch bar's record time (2026-01-05T12:00:41Z) at the same price, quantity and fee. | Same builder; it refuses to write unless the only false strict checks are `matched_within_tolerance` and `resting_tier_closes_verified`, with one `tier_close_out_of_tolerance` failure naming that close |
| `comparison_input*.json` | `go-trader.ledger-comparison-input` version 2 for each export, except the two version 4 resting inputs `comparison_input_paper_resting.json` and `comparison_input_paper_resting_touch.json`, which `source/build_paper_resting_fixture.py` writes and nobody edits by hand. Each segment is `loader_resolved` (the effective configuration) with stop provenance: `stop_evidence.raw_fields` from `source/config.json`, `resolved_fields` from the loader rules (`max_drawdown_pct` 50 is the perps type default), `leverage_origin` `config`, and verified absent `stop_defaults`. The manual segment's stop evidence is unverified because no stop model applies to it. | Version 2 inputs: edited by hand; rebind as below. The two version 4 inputs: rebuilt by their builder |
| `market/` | Frozen synthetic BTC 1h candles, hourly funding, venue meta and `manifest.json` (`offline_candle_manifest/v1`, window `comparison` = 2026-01-05T00:00Z to 2026-01-06T20:00Z, 60 warm-up bars). Not market data. | `uv run --no-sync python backtest/testdata/ledger_export/market/generate_market.py` (deterministic, no network) |
| `resting_market/` | The `market/` candles and funding with two highs raised: 2026-01-05T11:00Z from 62124.2 to 62125.0 (a touch of the first tier limit) and 2026-01-05T15:00Z from 61624.5 to 62127.0 (a two-tick wick with the close back inside). Open, low, close and volume are unchanged, so signals and pre-interval rows do not change. Not market data. | `uv run --no-sync python backtest/testdata/ledger_export/resting_market/generate_market.py` |
| `regime_market/` | Same comparison window with 300 warm-up bars, for regime gates whose live lookback is at least 200. The booked ledger stays on `market/`. Not market data. | `uv run --no-sync python backtest/testdata/ledger_export/regime_market/generate_market.py` |
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
`events` or `wallet_orphan_context` changed, or the export schema version
changed, rebind each comparison input (the snippet also writes the export
schema version, 2 since issue 1726), then rebuild the paper fixture with
`source/build_paper_fixture.py`:

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
    doc = json.load(open(d + exp))
    cin["export"]["booked_sections_sha256"] = lc.booked_sections_sha256(doc)
    cin["export"]["schema_version"] = doc["schema_version"]
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

- `export.json` + `comparison_input.json`: strict success with a modeled stop. `stop_loss_atr_mult: 0` and the resolved `max_drawdown_pct: 50` select the drawdown fallback, which arms at half the entry price on each simulated opening and never triggers. Two positions match (one with a partial and a full take-profit close, one open at the interval end with its later closes outside the interval), strategy funding matches as an unallocated total, a pre-interval position and the post-interval closes stay listed as outside the interval, and the synthetic terminal liquidation is excluded.
- `export_scale_in.json`: strict refusal (scale-in). Its verified fixed ATR stop is modeled and has no refusal. Booked conservation holds across an open, a scale-in, a gross partial close, a legacy full close, a legacy opening row, strategy funding and one row with no position id (unresolved). Approximate mode simulates with flat costs and is incomplete.
- `export_manual.json`: strict refusal; every event is not comparable.
- `export_paper.json` + `comparison_input_paper.json`: strict success with the paper cost model (`cost_model.kind` `paper_fill_cost_model`, version 1) at a relative price tolerance of `1e-9`. The tests derive the mixed-version refusal, the version 0 strict refusal and the mode-disagreement refusal from temporary copies.
- `export_paper_resting.json` + `comparison_input_paper_resting.json`: strict success with the resting take-profit rule on (capability row `modeled`, source `paper_strategy_flag`). The one simulated tier close carries the winning `resting_fill` (verdict `traded_through`, `fill_px` equal to the event price, limit 62125, reach 62127 in the 15:00 bar) and pairs with the booked partial close, which is `possible_tier`. The touch in the 11:00 bar books no simulated fill. The tests derive the rule-off (`resting_fill_unverified`, same `cost_model`), suppressed-close, conflict, `k_ticks`, `null` flag, version 3 input and `hl_sync_tp1_fill` cases from temporary copies.
- `export_paper_resting_touch.json`: the booked tier close is recorded in the touch bar's window, four bars before the trade-through bar, so the pair is out of tolerance and `resting_tier_closes_verified` fails with `tier_close_out_of_tolerance`. A booked touch fill recorded within one bar of a trade-through bar would still match inside the intrabar window; the comparison does not change that window.
- The committed exports carry no stop stamps, so initial stop geometry is `unavailable`. `backtest/tests/test_ledger_compare.py` proves agreement, an altered trigger, a later stamp, a missing attestation and an unmapped arm on temporary copies of `export.json`: it inserts synthetic `stop_loss_trigger_px` and `entry_atr` values (provenance kind `synthetic_test_evidence`), rebinds `booked_sections_sha256` and adds `initial_stop_geometry_evidence`. The committed capture and exports are unchanged.
