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
  "generated_at": "2026-06-19T00:00:00Z",
  "generator": "backtest/research/regime_1076_certify.py",
  "criteria": { "global_correction": "benjamini-hochberg", "fdr_q": 0.05, ... },
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

## Expiry / refresh

Each certified entry carries `expires_at` (`generated_at + default_ttl_days`,
default 90). Refresh by re-running the producer:

```bash
uv run --no-sync python backtest/research/regime_1076_certify.py
# narrower universe:
uv run --no-sync python backtest/research/regime_1076_certify.py \
    --symbols BTC/USDT,ETH/USDT --timeframes 1h,4h --classifiers adx,composite
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
