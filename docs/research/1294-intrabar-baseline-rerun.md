# Re-run of documented baselines under intra-bar stop resolution (#1294)

Generated: 2026-07-10. Cache: `shared_tools/trading_bot.db`, audit-dataset
last bars 2026-06-04 → 2026-06-12 — **byte-identical to the #1228/#1243 audit
snapshot**, so no cache-drift confound exists anywhere in this sweep.

PR #1292 (#1271) changed the backtester's default same-bar SL/TP race
resolution from bar-close detection to the intra-bar OHLC walk
(`intrabar_resolution="ohlc_walk"`). Separately, PR #1320 (#1315) switched the
`eval_windows.py` audit fee model from binanceus (0.1%/side) to hyperliquid
(0.045%/side). Every documented baseline predating those PRs was measured
under the old settings. This study re-measures the still-stale set and
attributes every delta between the two engine changes.

## Method — how deltas are attributed

Each reached harness was run TWICE on the identical cache, once per
`--intrabar-resolution` mode (the #983/#984 candidate drivers and the M6
`exit_policy_ab.py` harness now thread the flag; `eval_windows.py` already did
per #1292 review):

- **`bar_close` re-run vs documented numbers** = the #1320 fee-model change
  (cache is drift-free, so nothing else moves).
- **`ohlc_walk` re-run vs `bar_close` re-run** = the #1271 intra-bar change,
  isolated exactly.

#1271's only surface is the **engine-tracked SL trigger** (`stop_loss_atr_mult`
/ trailing / armed `sl_after` stops inside the `Backtester`). Close-evaluator
exits (tiered-TP ladders, open-signal-as-close) stay bar-close black boxes by
design, so studies without an engine stop are unreached by construction — and
every such study in this sweep verified byte-identical across the mode pair.

## Re-measured studies (dated 2026-07-10 addenda in each doc)

| study | doc | intrabar reach | outcome |
|---|---|---|---|
| #1181 HTF-filter baselines | `1181-htf-filter-baseline-revalidation.md` | none (no stops; binanceus platform → fee-unreached too) | reproduces to the digit in both modes; baselines current |
| #1054 regime_adaptive_htf M1 | `1054-regime-adaptive-htf-m1.md` | none (no stops) | noise gates bit-identical; M5 net -0.66 → -0.32/leg (pure fee model); deprecate stands |
| #983 squeeze close sweep | `backtest/candidates/squeeze_983/README.md` | **`sl_atr_1.5` / `trail_atr_3.0` / `tp_runner_trail3`** (engine-tracked stops); baseline + `tp_default` unreached (byte-identical) | all five M1 verdicts mode-identical; #1271 shifts the three engine-stop candidates' numbers without moving a verdict; fee model moved `tp_default` (158→94 trades, still collapses) and flipped `tp_runner_trail3` judged-OOS FAIL→PASS (flip present under `bar_close`, held-out 0/3, non-shipper); keep-baseline stands |
| #984 breakout close sweep | `backtest/candidates/breakout_984/README.md` | **`trail_atr_3.0` + `tp_tight_trail3`** (engine-tracked trailing stops); baseline + `tp_tight` unreached (byte-identical) | M1 protocol verdicts identical in both modes; the mode pairs isolate #1271 on `trail_atr_3.0` (#T 371→412, worstDD -50.13→-47.50%, Sharpe -0.288→-0.433) and `tp_tight_trail3` (pooled-IS Sharpe +0.09→-0.39, one window label flips, protocol verdict FAIL 0/3 unchanged); keep-baseline stands |
| #1152 ranging exit M6 | `1152-ranging-exit-geometry-m6.md` | **`b2_rv_wider` gate pair** (the incumbent B2 config's scalar `stop_loss_atr_mult: 1.5` is the engine stop; the un-re-measured ratchet runs are a separate group) | `bar_close` reproduces both documented verdicts (fee model only lifts one IS dataset over p<0.05); under `ohlc_walk` the lone single-style gate pass `mr.b2_rv_wider` downgrades `candidate_beats_incumbent` → `positive_but_not_significant` (one significant OOS contradiction appears) — a real #1271 effect, but the shipped decision (keep the collapsed group; gate requires both entry styles, squeeze fails) was already negative and stands |

**No shipped promotion or deprecation decision flips anywhere in the
re-measured set.** One gate *label* moved (#1152 `mr.b2_rv_wider`, above) — in
the conservative direction, on a candidate that was already blocked by the
cross-entry-style rule.

## Marked current without re-run (rationale)

- **#1315 M5 re-screen, #1282 limbo adjudication, #1277 ATR cutover** — already
  measured on post-#1271 geometry + #1320 fees (2026-07-10); they ARE the
  current baselines, out of scope here.
- **#1211 incumbent regime baseline** — regime-separation permutation
  statistics with no backtester/fee path (established in the #1243 addendum);
  #1271/#1320 provably cannot reach it.
- **#980, #981, #982, #985, #1166, #1167 M1/M4 strategy studies and the #1218
  vol-regime bake-off** — none arms an engine-tracked stop (all exit
  open-signal-as-close or via evaluator ladders; no `stop_loss_atr_mult` /
  trailing fields anywhere in their candidate specs), so #1271 is unreached by
  the same construction verified byte-for-byte on the stop-free runs above
  (#1181, #1054, and #983's `baseline`/`tp_default`). Their
  gross-edge / incumbent-relative gate verdicts are fee-free and unchanged;
  net columns shift by the #1320 fee model in the favorable direction (lower
  fees), which cannot un-justify a deprecate/negative verdict reached under
  the harsher fee model. Anyone re-opening one of these strategies for
  promotion should re-run its harness fresh (current defaults) rather than
  compare against the pre-#1292 net columns.

## Legacy-reproduction spot-check (acceptance)

`--intrabar-resolution bar_close` reproduction was verified against documented
numbers on two fronts: the #1181 tables reproduce **byte-for-byte** (both
modes), and the #1054 gross noise gates reproduce **bit-for-bit** (n=37 mean
+0.082%/trade p=0.3913; n=173 mean -0.022%/trade p=0.5516). Where `bar_close`
re-runs differ from documented numbers (983/984 nets), the entire difference
is the #1320 fee model, as designed — the pinned mode itself is intact.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
