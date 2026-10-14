# macro_event_breakout (#1654)

Research-only. `macro_event_breakout` is registered `edge_status="no_edge"`, `edge_source="study_inconclusive"` and
`production_refused=True`, futures only, with short entries. Discovery hides it. Every production path refuses it in
every mode, with or without `allow_no_edge`: config load, probe, read-only inspection and reload
(`scheduler/production_refusal.go`), the add-strategy command, and the Python check-script admission for solo and batch
checks and close-fallback references (`shared_tools/strategy_composition.py`). Promotion is a separate reviewed
decision. `REPORT.md` holds the verdict.

## Signal contract

- Calendar: a hash-pinned `macro_event_calendar/v1` artifact (owner `shared_tools/macro_event_inputs.py`). Each record
  carries event identity and type, revision, status, local time, IANA zone, the checked UTC conversion, `known_ms`
  (publication evidence), the source id and its own sha256. At a decision deadline only records with `known_ms` at or
  before the deadline count; the newest revision wins; two distinct records at that revision are a conflict.
- Range: the `range_bars` completed candles that close at or before the scheduled release. The selected record must
  be known before the release and unchanged at the deadline (`schedule_revised` otherwise). Postponed, cancelled and
  conflicting records hold.
- Entry: inside the `post_window_bars` window that starts at the first bar opening at or after the release, a long
  needs `confirm_bars` consecutive closes strictly above the range high; a short mirrors it below the low. One entry
  per event; a raw entry within `cooldown_bars` of an earlier raw entry holds. A bar N decision fills at bar N+1 open.
- Spread gate: the latest quote by event time among those received by the bar close, with event time within
  `max_quote_age_ms`; two records at that event time are a conflict. `(ask - bid) / ((ask + bid) / 2)` must be at
  most `max_spread_frac`. Missing, stale, conflicting or malformed quotes hold the entry. `spread_gate=false` is a
  research diagnostic only.
- Exit: `time_stop` with `max_bars = holding_bars` is the one close owner; `stop_loss_atr_mult` 1.0 is the stop owner.
  The evaluator owns no exit.
- Support: a decision reads at most `cooldown_bars + post_window_bars + range_bars + 1` rows (validated to fit the
  200-row check window), so truncated and full frames give the same last decision.

Every emitted row with an event records the event id, revision and record sha256, the calendar sha256, the release,
range times and values, window and confirmation times, the deadline, the selected quote (times, bid, ask, record
sha256, spread fraction) and the effective parameters (`meb_*` columns).

## Files

| file | role |
|---|---|
| `candidate_spec.json` | Frozen protocol: parameters, grid source, arms, windows, selection, costs, verdict rules and one pre-scoring amendment. |
| `calendar_sources.py` | `acquire` (network) freezes the official sources into `data/sources/`; `import` (offline) builds the calendar artifacts. |
| `data/sources/` | Hash-pinned bytes of the Federal Reserve tentative schedule releases (2022-06-24, 2023-06-23, 2024-08-09), the press-release index and the current FOMC calendar page. |
| `data/calendar/` | `fomc_calendar.json` (official), `fomc_matched_control.json` (research controls, never official releases) and `import_report.json`. |
| `data/venue/` | Hyperliquid 1h candles, funding, meta and one l2Book snapshot for the v4 venue manifests in `backtest/candidates/`. |
| `run_study.py` | `run` (offline, no network, no orders), `render`, `verify`, `protocol-hash`. |
| `results.json`, `REPORT.md` | Full outputs and the rendered report. |

Proxy candles and funding are the hash-pinned files in `backtest/candidates/chande_momentum_1661/data`, referenced in
place. No historical quote artifact exists for these windows.

## Reproduce

```
uv run --no-sync python backtest/candidates/macro_event_breakout_1654/calendar_sources.py import
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/macro_event_breakout_1654_venue_manifest.json
uv run --no-sync python backtest/candidates/macro_event_breakout_1654/run_study.py run
```

`calendar_sources.py import` rebuilds byte-identical calendar artifacts from the frozen sources. `run_study.py run`
rebuilds a fresh cache in a temporary directory, verifies every pinned hash, disables network acquisition and writes
the same `results.json` on every run of the same commit.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
