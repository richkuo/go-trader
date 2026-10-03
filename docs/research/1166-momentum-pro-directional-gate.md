# momentum_pro: regime-gating the short side via regime_directional_policy (#1166)

Generated: 2026-07-02

Follow-up to #980 (`docs/research/momentum_pro_980.md`), which found a real
but regime-dependent short edge: protocol OOS PASS 6/6, yet 0/6 legs beat the
bar in either bull held-out (2023, 2024). This measurement asks whether gating
the short side by regime state — coarse ADX 3-label first, composite 9-state
second, and the combined long+gated-short shape through the
`regime_directional_policy` machinery — recovers the bear-window edge without
bleeding it away in bull years. Full M1 protocol (#995); sibling of
`session_breakout` #1031 and `vol_momentum` #1021.

**Verdict: negative result across every gate shape measured.** The bull-year
failure is not separable by regime label for this entry style: momentum_pro's
own stacked-bearish-EMA + ADX entry gate is already a local-downtrend
detector, so every downtrend-shaped regime gate is nearly redundant with it
(≤3 of 45–60 bull-year shorts suppressed), and the one gate that does cut
exposure (composite clean-only, ~half the trades) leaves survivors that lose
at the same rate. The combined long+gated-short config improves held-outs to
2/3 — but an ungated same-engine control scores identically to the third
decimal, attributing the entire improvement to the long side riding bull
years. Nothing here is promotion evidence; no certification artifact ships;
live behavior is untouched.

## Harness change (the PR's code deliverable)

`eval_windows.py` now threads a `regime_directional_policy` candidate key
(#1025 shape `{trend_regime: {label: {direction, invert_signal?}}}`) through
`validate_candidate` → `evaluate_window` → `run_leg` → `Backtester`, mirroring
the `allowed_regimes` plumbing, plus a `--regime-directional-policy` JSON
flag:

- **Research-mode certified override.** The #1085 evidence gate defaults the
  policy OFF unless certified. A measurement leg passes the Backtester's
  `regime_directional_certified=True` input explicitly — bypassing the
  default-off live gate deliberately and visibly, never via a shipped
  certification artifact.
- **Engine-path guard.** Any policy state resolving `direction='both'` opens
  a two-sided book for that regime, which the plain signal path cannot model
  (the Backtester rejects both-states there) — `validate_candidate` requires
  `close_strategies` up front, mirroring the candidate-level
  `direction='both'` guard.
- **Byte-identical regression.** The new Backtester kwargs are added only
  when a policy is present; legs without one build identical kwargs
  (regression-tested by capturing constructor kwargs). `--list-json` verified
  byte-identical against `main` for both registries.
- Tests: accept/normalize (invert_signal defaulted), malformed-policy loud
  reject, both-state-without-close-refs reject, kwargs threading + certified
  override, plain-path behavioral side-switch, evaluate_window threading.

The isolated gated-short leg needs none of this: `allowed_regimes` +
`--direction short` (existing #1031 plumbing) already expresses
"short entries only in bear states, flat elsewhere". The policy threading is
what the combined shape requires (the policy vocabulary is
`long`/`short`/`both` — there is no flat state, so a bull-state `long` entry
admits longs rather than going flat).

## Reproduce

Data: the checkout's `shared_tools/trading_bot.db` OHLCV cache. **This cache
is NOT the one the #980 rows were produced on**: its 1h/4h series end
2026-06-04..2026-06-12 per dataset (vs #980's 2026-07-01), so the open-ended
`oos` window is shorter here and #980's published OOS rows are not
digit-comparable. Every comparison in this doc is against baselines re-run on
THIS cache (rows G and D below); the #980 rows match in shape (identical
window verdicts). Fixed windows (`is`, `2023`, `2024`, `2025H1`) verified
full-span via each leg's `span_days` — no silent truncation (#980's
cold-cache caveat). All runs `--registry spot`, the six audit datasets,
incumbent bar recomputed per (window, dataset).

```bash
# G — ungated short baseline (re-run of #980 mechanism 1 on this cache)
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot \
  --direction short --windows is,oos,2023,2024,2025H1

# A/B — isolated gated short, legacy ADX 3-label (B adds ranging; identical result)
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot \
  --direction short --allowed-regimes trending_down --windows is,2023,2024,2025H1
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot \
  --direction short --allowed-regimes trending_down --allowed-regimes ranging --windows is,2023,2024,2025H1

# E/F — isolated gated short, composite 9-state (#1058)
SPEC='{"medium": {"classifier": "composite", "period": 14}}'
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot \
  --direction short --allowed-regimes trending_down_clean --allowed-regimes trending_down_choppy \
  --regime-windows-spec "$SPEC" --windows is,2023,2024,2025H1
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot \
  --direction short --allowed-regimes trending_down_clean \
  --regime-windows-spec "$SPEC" --windows is,2023,2024,2025H1

# C — combined long+gated-short via the new policy threading (engine path)
cat > /tmp/mp1166-combined.json <<'J'
{
  "name": "momentum_pro",
  "direction": "both",
  "close_strategies": [{"name": "atr_stop", "params": {"atr_mult": 2.0, "atr_source": "entry"}}],
  "regime_directional_policy": {"trend_regime": {
      "trending_up": {"direction": "long"},
      "trending_down": {"direction": "both"}}}
}
J
uv run --no-sync python backtest/eval_windows.py --registry spot \
  --candidate-json /tmp/mp1166-combined.json --windows is,2023,2024,2025H1

# D — ungated same-engine control (identical minus the policy block)
# Final protocol-OOS look (after tuning-window selection): --windows oos on A and C.
```

The combined shape needs the open/close engine (`direction='both'` +
`close_strategies`), so C is compared against D — the same engine path, same
`atr_stop` close — never against the plain-path #980 rows. The M1 incumbent
bar itself stays the harness's fixed plain-path construct for all rows.

## Isolated gated short (plain path, `allowed_regimes` + `--direction short`)

Window mean Sharpe / mean DDadj (bar in parentheses), verdict per M1 #955:

| config | is | 2023 | 2024 | 2025H1 | oos |
|---|---|---|---|---|---|
| G ungated (#980 re-run) | 0.32 / 0.24 PASS | -1.01 / -0.74 FAIL | -0.97 / -0.73 FAIL | -0.08 / 0.20 PASS | 2.11 / 2.11 PASS |
| A adx `trending_down` | 0.29 / 0.22 PASS | -0.95 / -0.73 FAIL 0/6 | -1.01 / -0.74 FAIL 0/6 | -0.08 / 0.20 PASS | 2.03 / 2.00 PASS |
| B adx `trending_down`+`ranging` | identical to A in every leg | | | | not run |
| E comp `trending_down_clean`+`_choppy` | 0.25 / 0.16 PASS | -1.02 / -0.76 FAIL 0/6 | -1.08 / -0.77 FAIL 0/6 | -0.16 / 0.23 PASS | not run |
| F comp `trending_down_clean` | 0.59 / 0.70 PASS | -0.98 / -0.65 FAIL 0/6 | -1.04 / -0.57 FAIL 0/6 | -0.01 / 0.32 PASS | not run |

(bars: is -0.12 / -0.14 · 2023 1.46 / 3.67 · 2024 0.90 / 1.07 ·
2025H1 -0.42 / -0.37 · oos -0.75 / -0.49)

Suppressed-entry counts (total short entries across the six datasets;
suppression = G minus config — the gate produced the reduction, not data
absence; every window traded 6/6, no degenerate verdicts anywhere):

| window | G ungated | A adx | E comp family | F comp clean-only |
|---|---:|---:|---:|---:|
| is | 32 | 32 (−0) | 31 (−1) | 18 (−14) |
| 2023 | 47 | 45 (−2) | 44 (−3) | 24 (−23) |
| 2024 | 60 | 59 (−1) | 57 (−3) | 30 (−30) |
| 2025H1 | 29 | 29 (−0) | 28 (−1) | 10 (−19) |
| oos | 24 | 24 (−0) | — | — |

Two mechanisms, both fatal to the gating idea:

1. **Downtrend-family gates are redundant with the strategy's own entry
   gate.** momentum_pro shorts only fire from a stacked-bearish-EMA
   (20<50<200) regime with ADX>20 — by construction the bar-level regime
   classifier (ADX or composite trending_down family) labels those same bars
   trending-down. A/E suppress ≤3 of 45–60 bull-year entries; B (adding
   `ranging`) changes nothing at all because a ranging-labeled short never
   passes the internal ADX gate in the first place.
2. **The selective gate cuts exposure, not the loss rate.** F
   (`trending_down_clean` only) suppresses ~half the bull-year shorts
   (47→24, 60→30) yet 2023/2024 means barely move (-1.01→-0.98,
   -0.97→-1.04): the surviving "clean downtrend" entries lose exactly like
   the rejected ones. At entry time a bull-market correction is
   indistinguishable from a bear-market downtrend in these label vocabularies
   — the information that would separate them (what happens next) isn't in
   the regime state.

The gate is also costless where the edge lives (A keeps OOS PASS 6/6, zero
suppression) — it is simply inert: momentum_pro already self-gates.

## Combined long+gated-short (engine path, policy vs ungated control)

C = `direction both` + `atr_stop` 2.0 (entry-ATR) + policy
`{trending_up: long, trending_down: both}` (ranging → base `both` fallback).
D = identical minus the policy — the control that isolates the gate's
contribution.

| config | is | 2023 | 2024 | 2025H1 | oos |
|---|---|---|---|---|---|
| D ungated control | 0.10 / -0.07 PASS | 2.25 / 8.72 PASS | 0.67 / 1.26 FAIL | -0.13 / -0.20 PASS | 1.73 / 1.75 PASS |
| C policy-gated | 0.08 / -0.11 PASS | 2.25 / 8.72 PASS | 0.69 / 1.32 FAIL (4/6 S, 3/6 D) | -0.14 / -0.21 PASS | 1.73 / 1.75 PASS |

- **C ≡ D everywhere that matters.** 2023 rows identical to the digit (six
  legs, 1 trade each — the long entry; both configs took zero 2023 shorts
  because the internal gate already suppressed them). OOS legs identical to
  the third decimal on all six datasets (both 17 trades). Total entries
  differ by ≤2 per window (2024: 48 vs 46).
- Adjacent policy shapes converge to the same rows: `{trending_up: long}`
  alone (trending_down→both via base fallback — semantically identical,
  confirms the resolver) and the tighter `{trending_up: long, ranging: long,
  trending_down: both}` (ranging shorts already blocked internally). The
  "plateau" (M1 step 6) is real but degenerate — it is the plateau of a
  no-op.
- The held-out improvement over #980's short row (1/3 → 2/3) and the 2024
  0/6 → 4/6-Sharpe movement are therefore **entirely the long side riding
  bull years** (2023: +79..+815% per leg from the long entries), not the
  directional gate. Against the issue's pass bar — "the GATED config must
  keep protocol-OOS PASS and stop losing to the bar 0/6 in 2023/2024" — the
  literal clauses are met by C, but the D control shows the gate contributed
  none of it; crediting the policy would be attributing the control's result
  to the treatment. 2024's window verdict also remains FAIL on means.

## Verdict table

| config | protocol OOS | held-out | disposition |
|---|---|---|---|
| G short ungated (#980 re-run) | PASS | 1/3 (0/6 both bull years) | baseline reproduced on this cache |
| A/B short + ADX bear gate | PASS (A) | 1/3 (0/6 both bull years) | negative — gate suppresses ≤2 bull-year entries |
| E short + composite bear family | not taken | 1/3 tuning shape (0/6 both bull years) | negative — same redundancy |
| F short + composite clean-only | not taken | 1/3 tuning shape (0/6 both bull years) | negative — halves trades, survivors lose identically |
| C combined via policy | PASS | 2/3 | attribution fails: D control identical — improvement is the long side |
| D combined ungated control | PASS | 2/3 | the same result with no directional machinery at all |

**Disposition: negative result — documented, not deleted; close #1166.**
No regime state in either vocabulary separates momentum_pro's bear short
edge from its bull failure, because the strategy's entry condition already
conditions on the same information. No certification evidence is generated,
`regime_directional_policy` remains default-off/uncertified for momentum_pro
(#1085), no registry defaults change, and live behavior is untouched. The
harness threading stays: it is strategy-agnostic and lets the next
directional-gate candidate (one whose entries do NOT already self-select for
local downtrends) be measured on the M1 bar without new plumbing.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
