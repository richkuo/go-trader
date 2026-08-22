# #1085 — Evidence-gated directional certification

Follow-up to the #1076 negative result (see `README_1076_directional_premise.md`).
`regime_directional_policy` (#779) bets a live HL-perps strategy long/short on the
current regime label. #1076 showed that premise is empirically false across the
tested universe (0/2121 per-state forward-return tests survive global
FDR/Bonferroni; 0/60 after the block-shuffle placebo). #1084 shipped a `[WARN]`.
This is the principled end-state: the directional-selection surface is
**default-off** and resolves to the strategy's base direction, and is honored for
a strategy **only** where a per-`(asset, timeframe, classifier)` certification
gate proves real, multiplicity-honest directional edge.

Nothing in the tested universe currently qualifies, so the shipped artifact
certifies nothing and every `regime_directional_policy` runs default-off.

**Latest run: 2026-08-22 (#1443)** — full default universe plus HYPE sourced from
Hyperliquid, one invocation, `n_perm=30000`, `seed=0`. 0 of 16 screened cells
certify; every cell fails at the global-BH gate. At this permutation count the
p-floor (3.333e-05) sits *below* the rank-1 BH critical value (3.791e-05) over the
1319-row family, so a single genuinely-separating state was arithmetically able to
pass — it is a real negative, not a resolution artifact. Per-cell verdicts, the
HYPE window-coverage limit and the two target cells are written up in
`README_1076_directional_premise.md` § "Refreshed run — 2026-08-22"; the machine
-readable form is `regime_1443_run_report.json`.

The `[WARN]` the three configured deployment strategies print at config load is
therefore the **expected, verified** outcome. The configured policy blocks are
kept with this record rather than removed; see the same section for why.

## Single source of truth

The statistical test lives in ONE place — the Python research harness. Go never
reimplements it; it consumes a data artifact.

| Piece | Path |
|---|---|
| Producer (re-runs the #1076 screen, applies the gate, emits the artifact) | `backtest/research/regime_1076_certify.py` |
| Artifact (the certified set — SSoT) | `backtest/research/regime_directional_certifications.json` |
| Live consumer (Go) | `scheduler/regime_directional_certification.go` |
| Backtest consumer (parity) | `backtest/directional_certification.py` |

`normalize_cert_asset` and the `(asset, timeframe, classifier)` key shape are kept
byte-identical between the Go and Python sides so both reconcile a live `BTC` arg
and a research `BTC/USDT` symbol to the same key.

## Certification criterion

A `(asset, timeframe, classifier)` cell is certified for a canonical trend
direction only when a directional state for that direction:

1. survives **global** Benjamini-Hochberg FDR (q=0.05) across the *whole*
   directional family — not the within-cell BH the screen also reports;
2. is **sign-aligned** with the policy bet (`trending_up → long`,
   `trending_down → short`); and
3. persists in a **held-out forward** window (`is`/`oos`) — the windows the live
   policy must actually work in; a historical-only hit is overfit.

## Artifact schema

```json
{
  "schema_version": 1,
  "generated_at": "2026-08-22T00:00:00Z",
  "generator": "backtest/research/regime_1076_certify.py",
  "criteria": {
    "global_correction": "benjamini-hochberg",
    "fdr_q": 0.05,
    "require_sign_aligned": true,
    "require_held_out_forward": true,
    "held_out_windows": ["is", "oos"],
    "screened_family_size": 1319,
    "n_perm": 30000,
    "permutation_p_floor": 3.3332e-05,
    "data_sources": { "BTC/USDT": "binanceus", "HYPE/USDC:USDC": "hyperliquid" },
    "universe": { "symbols": [...], "timeframes": [...], "windows": [...],
                  "classifiers": [...], "horizons": [...] }
  },
  "default_ttl_days": 90,
  "certified": [
    {
      "asset": "BTC", "timeframe": "1h", "classifier": "composite",
      "generated_at": "...", "expires_at": "...",
      "states": { "trending_up": "long", "trending_down": "short" }
    }
  ]
}
```

`certified` is currently `[]`.

## Per-symbol data sources — `--symbols SYMBOL[@exchange]` (#1443)

The screen loads OHLCV from one default exchange (`eval_windows.PLATFORM`,
`binanceus`). An asset that does not trade there was previously unreachable, so
the `--symbols` contract now takes an optional per-symbol source:

```
BTC/USDT                     -> binanceus (the PLATFORM default)
HYPE/USDC:USDC@hyperliquid   -> hyperliquid
```

The spec splits on the **last** `@`, so ccxt symbols carrying `/` and `:` parse
correctly. An empty symbol or empty exchange raises instead of falling back — a
typo must never quietly screen the wrong series. Both `regime_1076_certify.py`
and `regime_1076_directional_premise.py` share the one parser
(`premise.parse_symbols_arg`).

**`PLATFORM` is never repointed.** The #1315 axis separation pins it because
every committed regime baseline was computed on that series; a mapped symbol's
rows cache under its own `exchange_id` namespace in the storage layer, so the
`binanceus` baselines cannot be disturbed. Boundary decision for HYPE (issue
Approach 5): the Hyperliquid rows stay a **cert-run cache entry** under
`exchange_id="hyperliquid"` — not a general storage fixture, and not a new
platform axis.

**Window clipping.** `load_cached_data`'s empty-cache fallback fetches from
`since=start_date` and returns the whole fetched history *unsliced*. For an asset
whose listing post-dates a window, that would screen later data under the earlier
window's label. `_load` now clips the frame to the window before labeling; on the
cached path the clip is a no-op, so every already-screened cell is unchanged.

## Family integrity — the narrowed-run refusal (#1443)

`certify()` applies global Benjamini-Hochberg over **only the rows of the current
invocation**, so the BH family *is* the run's universe. Narrowing any axis
shrinks the family, and a smaller family resolves a smaller effect — the
pooled-limit trap #1424 records for the Hurst gate. A run whose universe is not a
superset of the producer defaults (`DEFAULT_SYMBOLS` × `DEFAULT_TIMEFRAMES` ×
`DEFAULT_WINDOWS` × `DEFAULT_CLASSIFIERS` × `DEFAULT_HORIZONS`) therefore
**refuses to write the repo artifact**, before touching any data:

```
REFUSING to write .../regime_directional_certifications.json: narrowed family: ...
```

`--allow-narrowed-family` overrides the refusal for research-only artifacts
written elsewhere; the warning prints either way. Adding symbols is always fine —
a superset only raises the bar.

Note the producer's default universe (BTC/ETH/SOL × 1h/4h) is narrower than the
full #1076 battery, which also covered BTC 15m/30m/2h and BNB/XRP 4h through
`regime_1076_aggregate.py`. The guard pins the *producer's* own default axes; it
does not reconstruct the aggregate battery.

## Run provenance in `criteria` (#1443)

Every artifact records what the gate actually corrected against:

| Key (inside `criteria`) | Meaning |
|---|---|
| `screened_family_size` | directional rows the global BH ran over — computed FROM the rows, never claimed |
| `data_sources` | `{symbol: exchange_id}` resolved for the run |
| `universe` | the symbols / timeframes / windows / classifiers / horizons axes |
| `n_perm` | block-shuffle permutation count |
| `permutation_p_floor` | `1/(n_perm+1)` — the smallest p-value the test can emit |

A narrowed or mis-sourced artifact is now detectable by inspection: a
`screened_family_size` far below the full-screen scale is a red flag.

**All of this nests inside `criteria` on purpose.** The Go loader parses with
`DisallowUnknownFields` and `Criteria` is the only free-form map in the schema, so
a new **top-level** key would make the live daemon fail closed on a perfectly
valid artifact. `scheduler/regime_directional_certification_test.go` pins both
halves of that contract. `schema_version` stays `1`.

### Permutation resolution vs the evidence bar

`per_state_significance` computes `p = (ge+1)/(n_perm+1)`, so its smallest
possible p-value is `1/(n_perm+1)`. The rank-1 global-BH critical value is
`q/m`. When the floor sits **above** `q/m`, no single row can certify at any
effect size, and an empty artifact is not evidence of absence. The producer
prints both numbers on every run and records them in the run report, so the
verdict's resolution is stated rather than assumed. The evidence bar itself
(`q=0.05`) is untouched — raising `n_perm` only sharpens resolution.

## Per-cell run report (#1443)

`--report-out` (default `regime_1443_run_report.json`) writes the verdict for
**every screened cell**, not only the certified ones, with the first criterion it
fails in gate order:

`no_directional_rows` → `fails_global_bh` → `wrong_signed` →
`not_held_out_forward` → `certified`

Each entry carries the cell's best p-value, its BH rank and the critical value it
needed, plus per-`(symbol, timeframe, window)` coverage and the windows that
contributed nothing. The artifact schema is unchanged and still lists only
certified cells.

## Expiry / refresh

Each certified entry carries `expires_at` (`generated_at + default_ttl_days`,
default 90). Refresh by re-running the producer:

```bash
# Full default universe:
uv run --no-sync python backtest/research/regime_1076_certify.py

# Full default universe PLUS an HL-sourced asset, one invocation (#1443):
uv run --no-sync python backtest/research/regime_1076_certify.py \
    --symbols "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid" \
    --n-perm 30000 --seed 0

# Research-only, narrowed, written elsewhere:
uv run --no-sync python backtest/research/regime_1076_certify.py \
    --symbols BTC/USDT --timeframes 1h --allow-narrowed-family \
    --out /tmp/research_cert.json
```

The live daemon reloads the artifact at startup and on SIGHUP (path overridable
via `GO_TRADER_DIRECTIONAL_CERT_PATH`).

## Safety model (the hard part)

- **Default-off / fail-closed.** A missing, malformed, or expired certification
  yields "not certified" → base direction. A malformed artifact is loud but never
  fatal — taking down live trading over a research sidecar is the less-safe
  outcome.
- **From-flat migration only (req 1).** The live entry gate keys on the live
  verdict **only when flat**. An open position rides under the certification
  status stamped at its open (`Position.DirectionCertifiedAtOpen`). Disabling /
  decertifying with an open position is surfaced, never silently flipped: for a
  sole-owner coin the #822 orphan-close migrates the position to base; for a
  **shared coin** the conflict is escalated to the operator (CRITICAL + owner DM)
  because a reduce-only close would touch live peers — it must be closed
  manually.
- **Expiry/refresh never disturbs an open position (req 2).** Because the open
  position rides under its open-time stamp, a time-based expiry or an artifact
  refresh can never flip its effective direction mid-position or trip the
  orphan-close. Expiry is advisory while open, enforced only from flat.
- **Backtest/live parity (req 3).** The backtester applies the same
  `(asset, timeframe, classifier)` gate (classifier = the one the backtester
  actually models — composite if a windows spec is configured, else legacy ADX),
  so a backtest can never show a directional edge the live path suppresses.

---
Created with LLM: Opus 4.8 | high | Harness: Claude Code
