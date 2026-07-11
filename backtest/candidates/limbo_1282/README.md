# limbo_1282 — BH family specs for the M5-limbo adjudication (#1282)

Spec-only study (no drivers): three `auto_suggest.py` specs that put each
candidate family from the M5-limbo set through one Benjamini–Hochberg pass,
per the #1210 correction-scope rule. Findings, verdicts, and the full
adjudication pipeline (short-leg screens, wide-pool noise gates, M1 protocol)
are recorded in `docs/research/1282-m5-limbo-verdicts.md`.

- `suggest_breakout.json` — the #984/#1165 arms (baseline,
  `comp_up_clean_p21`, `m4_bear_selective`) on the futures registry.
  Result: `comp_up_clean_p21` reproduces its #1165 pass (is+oos+2025H1).
- `suggest_mean_reversion_pro.json` — the #981 frequency knobs
  (`touch_entry`/`turn_entry`, both, baseline). Result: all
  `incumbent_stands`; knobs stay default-off.
- `suggest_chart_pattern.json` — the #982 HTF gate factor family
  (f0/f4/f5/f6). Result: `gate_f4` is the sole M1 survivor (4/5 windows),
  re-affirming the #982 default-off opt-in under BH.

Run:

```
uv run --no-sync python backtest/auto_suggest.py --spec backtest/candidates/limbo_1282/suggest_<family>.json
```

Run artifacts land in `<study>_runs/` next to the spec (not committed;
reproducible from the specs on the shared OHLCV cache).
