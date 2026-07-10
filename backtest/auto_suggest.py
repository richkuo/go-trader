#!/usr/bin/env python3
"""#1210: Generalized M1-M6 auto-suggester — sweep candidates, rank gate-survivors.

The M1-M6 research harnesses grade a candidate change you hand them; they do not
search. Every retune (e.g. ``regime_1152_exit_retune.py``) re-authors the same
driver skeleton — candidate enumeration -> parallel harness shelling -> window
rollup -> promotion gate — from scratch. This module extracts that skeleton once
and adds the one thing a hand-rolled driver keeps omitting: a sweep-wide
multiple-comparisons correction, so mining N candidates cannot surface a lucky
p<.05 as a "survivor".

SUGGEST-ONLY — HARD BOUNDARY. This tool NEVER writes a live default
(``ratchetTierGroupDefaults`` / ``regimeTPTierGroupDefaults`` / any stop field),
never opens the live config, never shells git/gh, and has no apply/promote/PR
surface. It emits a ranked report with per-dataset evidence and the exact
reproduction command; a human makes every promotion call (the #1152 study is the
cautionary case — 22 disciplined candidates, ``mr.b2_rv_wider`` even cleared the
pre-registered gate, and the human still correctly held it).

Given a declarative spec (see ``backtest/candidates/<study>/suggest.json``) it:
  1. expands a candidate space (explicit candidate-json files + optional param
     sweeps + gate variants + M2 close-stack grids),
  2. fans each candidate across the appropriate M-harnesses (respecting each
     harness's preconditions — the M1 step-2 noise gate runs BEFORE selectivity
     work; non-replayable M6 closes are excluded, mirroring
     ``_REPLAYABLE_CLOSE_NAMES``),
  3. corrects the family of primary p-values (M1 step-2 permutation + M6 paired
     Wilcoxon) with Benjamini-Hochberg — M3/M5/MC emit no p-values and pass
     through as clearly-labeled uncorrected context,
  4. ranks survivors first and writes a shortlist artifact.

The Monte Carlo columns (#1295, the ``mc`` harness) are ADVISORY in the strong
sense: they never gate a promotion whether the run succeeds, fails, or is
skipped. See ``ADVISORY_HARNESSES`` / ``gate_relevant_results`` — "the gate does
not read the mc key" would NOT have been sufficient, because the failed-run scan
reads the results dict's VALUES.

Usage:
  uv run --no-sync python backtest/auto_suggest.py \
      --spec backtest/candidates/squeeze_momentum_1198/suggest.json \
      [--jobs 4] [--out-dir /tmp/suggest] [--only KEY[,KEY...]] \
      [--windows is,oos] [--datasets BTC/USDT:1h,...] [--alpha 0.05] \
      [--dry-run] [--json OUT.json] [--markdown OUT.md]
"""
from __future__ import annotations

import argparse
import copy
import json
import os
import shlex
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_THIS_DIR, ".."))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)

from eval_windows import (  # noqa: E402  (path set above)
    DATASETS as M1_DATASETS,
    WINDOWS as M1_WINDOWS,
    expand_sweep,
    validate_candidate,
)
from exit_policy_ab import candidate_is_replayable  # noqa: E402
from regime_stats import benjamini_hochberg  # noqa: E402

# Harness paths, relative to the repo root (reproduction commands print these)
# and absolute (subprocess spawns use these).
HARNESS_REL = {
    "m1_noise": "backtest/gross_edge_noise.py",
    "m1": "backtest/eval_windows.py",
    "m3": "backtest/exit_diagnostics.py",
    "m5": "backtest/fee_audit.py",
    "m6": "backtest/exit_policy_ab.py",
    "mc": "backtest/monte_carlo.py",
}
HARNESS_ABS = {k: os.path.join(_REPO, v) for k, v in HARNESS_REL.items()}

KNOWN_HARNESSES = set(HARNESS_REL)
OPEN_HARNESSES = ("m1_noise", "m1", "m3", "m5", "mc")  # applied to open candidates
DEFAULT_HARNESSES = ["m1_noise", "m1", "m3", "m5", "mc"]
KNOWN_REGISTRIES = ("spot", "futures")

# ADVISORY harnesses (#1295) emit no p-value AND must never influence the
# promotion gate IN ANY STATE — including a failed run. This is strictly
# stronger than "candidate_verdict does not read the key": both
# ``candidate_verdict`` and ``main`` scan ``results.values()`` generically for a
# failed status, so an advisory harness that merely APPEARS in the results dict
# would demote a genuine survivor to ``run_failed`` and flip the exit code.
# Every gate-facing scan goes through ``gate_relevant_results`` instead.
#
# M3/M5 are deliberately NOT listed. They are uncorrected CONTEXT for the
# reader, but a failed M3/M5 run has always marked the candidate run_failed and
# that pre-#1295 gate behavior is preserved byte-for-byte.
ADVISORY_HARNESSES = ("mc",)

# monte_carlo.py's own defaults, restated because auto_suggest passes them
# explicitly into the argv tail.
MC_DEFAULT_N_PATHS = 10000
MC_SCHEME_FOR_COLUMNS = "permute"  # pure sequencing risk; block kept in evidence

# Verdict vocabulary (ranked best-to-worst for the shortlist).
VERDICT_ORDER = [
    "survivor",
    "positive_uncorrected_only",
    "positive_but_not_significant",
    "incumbent_stands",
    "inconclusive",
    "noise_gate_blocked",
    "excluded_not_replayable",
    "run_failed",
]

FOOTER = ("Suggest-only. No config was modified and no live default was written. "
          "Promotion is a human decision.")


# ===========================================================================
# Pure — spec model and candidate expansion (unit-tested without data access)
# ===========================================================================

def _resolve_ref(ref, spec_dir: str) -> dict:
    """A candidate/config ref is either an inline dict or a filename resolved
    against the spec's own directory (the ``backtest/candidates/<study>/``
    convention)."""
    if isinstance(ref, dict):
        return copy.deepcopy(ref)
    if isinstance(ref, str):
        path = ref if os.path.isabs(ref) else os.path.join(spec_dir, ref)
        with open(path) as fh:
            return json.load(fh)
    raise ValueError(f"expected an inline object or a filename, got {ref!r}")


def load_spec(raw: dict, spec_dir: str) -> dict:
    """Validate/normalize a suggest.json spec, resolving file refs against
    ``spec_dir`` and running every open candidate through the M1 validator so a
    malformed candidate fails loudly here rather than deep in a subprocess."""
    if not isinstance(raw, dict):
        raise ValueError("spec must be a JSON object")

    registry = str(raw.get("registry") or "spot").strip().lower()
    if registry not in KNOWN_REGISTRIES:
        raise ValueError(f"registry must be one of {KNOWN_REGISTRIES}, got {registry!r} "
                         f"(M6 is spot/futures only; perps backtest via the futures registry)")

    harnesses = list(raw.get("harnesses") or DEFAULT_HARNESSES)
    unknown = [h for h in harnesses if h not in KNOWN_HARNESSES]
    if unknown:
        raise ValueError(f"unknown harnesses {unknown}; known: {sorted(KNOWN_HARNESSES)}")

    windows = list(raw.get("windows") or ["is", "oos"])
    bad_windows = [w for w in windows if w not in M1_WINDOWS]
    if bad_windows:
        raise ValueError(f"unknown windows {bad_windows}; known: {list(M1_WINDOWS)}")

    correction = dict(raw.get("correction") or {})
    method = str(correction.get("method") or "benjamini_hochberg")
    if method != "benjamini_hochberg":
        raise ValueError("correction.method must be 'benjamini_hochberg' "
                         "(the only supported family correction, per #1210 scope)")
    alpha = float(correction.get("alpha", 0.05))

    base = _resolve_ref(raw["base"], spec_dir) if raw.get("base") is not None else None

    candidates = []
    for c in raw.get("candidates") or []:
        if "key" not in c or "candidate" not in c:
            raise ValueError("each candidates[] entry needs 'key' and 'candidate'")
        cand = _resolve_ref(c["candidate"], spec_dir)
        # Fail-loud validation on a copy — validate_candidate mutates in place
        # (normalizes regime_windows_spec), and we keep the RAW dict so the M1
        # subprocess re-validates from source.
        validate_candidate(copy.deepcopy(cand))
        candidates.append({
            "key": c["key"],
            "candidate": cand,
            "harnesses": list(c.get("harnesses") or harnesses),
            "hypothesis": c.get("hypothesis"),
        })

    # ---- mc (#1295): advisory Monte Carlo block -----------------------------
    # The threshold source mirrors monte_carlo.py's own mutual exclusion: an
    # explicit percentage, OR a live config + strategy id to resolve the
    # per-strategy max_drawdown_pct hierarchy — never both. Neither => the
    # harness default (25, the portfolio kill-switch default).
    mc = dict(raw.get("mc") or {})
    if mc:
        unknown_mc = set(mc) - {"kill_switch_pct", "config", "strategy_id",
                                "n_paths"}
        if unknown_mc:
            raise ValueError(f"unknown mc keys {sorted(unknown_mc)}; known: "
                             "kill_switch_pct, config, strategy_id, n_paths")
        if mc.get("kill_switch_pct") is not None and mc.get("config"):
            raise ValueError("mc block sets both 'kill_switch_pct' and "
                             "'config'; they are mutually exclusive threshold "
                             "sources (monte_carlo.py refuses both)")
        if bool(mc.get("config")) != bool(mc.get("strategy_id")):
            raise ValueError("mc 'config' and 'strategy_id' go together (the "
                             "strategy id selects whose max_drawdown_pct "
                             "hierarchy resolves)")
        if mc.get("config"):
            cfg = mc["config"]
            mc["config"] = (cfg if os.path.isabs(cfg)
                            else os.path.join(spec_dir, cfg))
        if mc.get("n_paths") is not None and int(mc["n_paths"]) < 1:
            raise ValueError("mc 'n_paths' must be >= 1")

    m6 = None
    if raw.get("m6") is not None:
        m6 = dict(raw["m6"])
        bc = m6.get("baseline_config")
        if bc and isinstance(bc, str):
            m6["baseline_config"] = (bc if os.path.isabs(bc)
                                     else os.path.join(spec_dir, bc))
        # M6 resolves the incumbent from EXACTLY one of a live-daemon
        # baseline-config (resolve the strategy's live close) or an explicit
        # incumbent_close ladder — mirroring exit_policy_ab, which rejects both
        # and neither. The explicit path lets a spec exercise M6 self-contained,
        # without authoring a v15 config fixture.
        if bool(m6.get("baseline_config")) == bool(m6.get("incumbent_close")):
            raise ValueError(
                "m6 block needs EXACTLY one of 'baseline_config' (resolve the "
                "live close from a daemon config) or 'incumbent_close' (an "
                "explicit close-ref ladder); got "
                + ("both" if m6.get("baseline_config") else "neither"))
        # strategy_id is embedded unconditionally into every M6 argv
        # (m6_argv_tail's leading --strategy) on BOTH incumbent paths — the
        # baseline_config path uses it to select the strategy inside the
        # config, the incumbent_close path uses it as the open-strategy name.
        # Resolution is per-variant with an m6-level default
        # (_exit_ab_entry: variant.get("strategy_id") or m6.get("strategy_id")),
        # so fail loudly HERE at load time if any variant would resolve to
        # None rather than surfacing as a broken '--strategy None' subprocess.
        if not m6.get("strategy_id"):
            if m6.get("close_stack_specs"):
                raise ValueError(
                    "m6 'close_stack_specs' are generated variants that cannot "
                    "carry a per-variant 'strategy_id'; set an m6-level "
                    "'strategy_id' (the open-strategy name, or the config's "
                    "strategy id when using 'baseline_config').")
            for v in m6.get("candidate_close_variants") or []:
                if not v.get("strategy_id"):
                    raise ValueError(
                        "m6 candidate_close_variant "
                        + repr(v.get("key") or "<unkeyed>")
                        + " has no 'strategy_id' and the m6 block sets no "
                        "default; add an m6-level 'strategy_id' or a per-variant "
                        "override (the open-strategy name, or the config's "
                        "strategy id when using 'baseline_config').")

    return {
        "study": str(raw.get("study") or "unnamed_study"),
        "registry": registry,
        "harnesses": harnesses,
        "windows": windows,
        "datasets": raw.get("datasets"),  # None -> audit six
        "correction": {"method": method, "alpha": alpha},
        "base": base,
        "candidates": candidates,
        "sweep": raw.get("sweep"),
        "gate_variants": raw.get("gate_variants"),
        "m6": m6,
        "mc": mc,
        "spec_dir": spec_dir,
    }


def _sanitize_label(label: str) -> str:
    """expand_sweep emits 'kc_mult=1.3 mom_lookback=12'; make it key/path safe."""
    return label.replace("=", "").replace(" ", ".").replace("/", "_")


def _open_entry(key: str, candidate: dict, harnesses: list, hypothesis) -> dict:
    limitations = []
    # fee_audit (M5) screens registry defaults only — it has no --params surface,
    # so a swept/params candidate cannot be audited at its own params.
    if "m5" in harnesses and candidate.get("params"):
        limitations.append("m5_params_unaudited")
    return {
        "key": key,
        "kind": "open",
        "candidate": candidate,
        "harnesses": [h for h in harnesses if h in OPEN_HARNESSES],
        "hypothesis": hypothesis,
        "precondition_errors": [],
        "limitations": limitations,
    }


def _exit_ab_entry(key: str, variant: dict, m6: dict) -> dict:
    close_refs = variant.get("candidate_close")
    errors = []
    if not candidate_is_replayable(close_refs):
        # Mirrors M6's own refusal: a non-self-contained close (open-as-close,
        # signal-reversal, per-tick regime variant) has no rule to replay per
        # entry, so it is excluded rather than run and fabricated.
        errors.append("excluded_not_replayable")
    return {
        "key": key,
        "kind": "exit_ab",
        "candidate": {
            "baseline_config": m6.get("baseline_config"),
            "incumbent_close": m6.get("incumbent_close"),
            "strategy_id": variant.get("strategy_id") or m6.get("strategy_id"),
            "candidate_close": close_refs,
            "candidate_stops": variant.get("candidate_stops", "inherit"),
            "allowed_regimes": list(variant.get("allowed_regimes") or []),
        },
        "harnesses": ["m6"],
        "hypothesis": variant.get("hypothesis"),
        "precondition_errors": errors,
        "limitations": [],
    }


def expand_candidates(spec: dict) -> list:
    """Expand the declarative spec into concrete candidate entries.

    Sources: explicit ``candidates`` (fully-formed, gate baked in) + an optional
    ``base`` x ``sweep`` (expand_sweep) x ``gate_variants`` grid + an optional
    ``m6`` exit-A/B block (its own close variants, plus M2 ``close_stack_specs``
    expanded via optimizer.generate_close_stack_grid). Keys are deterministic;
    duplicates are rejected."""
    entries = []
    default_harnesses = spec["harnesses"]

    for c in spec["candidates"]:
        entries.append(_open_entry(c["key"], c["candidate"],
                                   c["harnesses"], c["hypothesis"]))

    sweep, gate_variants, base = spec.get("sweep"), spec.get("gate_variants"), spec.get("base")
    if sweep or gate_variants:
        if base is None:
            raise ValueError("sweep/gate_variants require a 'base' candidate")
        base_name = base.get("name", "base")
        if sweep:
            sweep_specs = [(k, list(v)) for k, v in sweep.items()]
            seeds = []
            for label, params in expand_sweep(dict(base.get("params") or {}), sweep_specs):
                cand = copy.deepcopy(base)
                cand["params"] = params
                seeds.append((_sanitize_label(label), cand))
        else:
            seeds = [(None, copy.deepcopy(base))]

        for slabel, scand in seeds:
            for gv in (gate_variants or [None]):
                cand = copy.deepcopy(scand)
                parts = [base_name]
                if slabel:
                    parts.append(slabel)
                if gv:
                    if not gv.get("allowed_regimes"):
                        raise ValueError("gate_variants[] entry needs 'allowed_regimes'")
                    cand["allowed_regimes"] = list(gv["allowed_regimes"])
                    if gv.get("regime_windows_spec"):
                        cand["regime_windows_spec"] = gv["regime_windows_spec"]
                    parts.append(gv["label"])
                validate_candidate(copy.deepcopy(cand))
                entries.append(_open_entry(".".join(parts), cand, default_harnesses, None))

    m6 = spec.get("m6")
    if m6:
        variants = list(m6.get("candidate_close_variants") or [])
        stack_specs = m6.get("close_stack_specs")
        if stack_specs:
            from optimizer import generate_close_stack_grid
            for i, stack in enumerate(generate_close_stack_grid(stack_specs)):
                close_refs = list(stack.get("close_strategies") or [])
                if stack.get("stop_loss_atr_mult"):
                    close_refs.append({"name": "stop_loss_atr_mult",
                                       "params": {"atr_mult": stack["stop_loss_atr_mult"]}})
                elif stack.get("trailing_stop_atr_mult"):
                    close_refs.append({"name": "trailing_stop_atr_mult",
                                       "params": {"atr_mult": stack["trailing_stop_atr_mult"]}})
                variants.append({
                    "key": f"close_stack_{i}",
                    "candidate_close": close_refs,
                    "allowed_regimes": m6.get("allowed_regimes"),
                })
        for v in variants:
            if "key" not in v:
                raise ValueError("each m6 candidate_close_variants[] entry needs 'key'")
            entries.append(_exit_ab_entry(f"m6.{v['key']}", v, m6))

    seen = set()
    for e in entries:
        if e["key"] in seen:
            raise ValueError(f"duplicate candidate key: {e['key']}")
        seen.add(e["key"])
    return entries


# ===========================================================================
# Pure — per-harness argv tails (the caller prepends [python, harness path])
# ===========================================================================

def _csv(items) -> str:
    return ",".join(items)


def m1_argv_tail(candidate_path, registry, windows, datasets, out_json) -> list:
    tail = ["--candidate-json", candidate_path, "--registry", registry,
            "--windows", _csv(windows), "--json", out_json]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    return tail


def noise_argv_tail(strategy, params_json, registry, direction, windows,
                    datasets, resamples, seed, alpha, out_json) -> list:
    tail = ["--strategy", strategy, "--registry", registry,
            "--windows", _csv(windows), "--resamples", str(resamples),
            "--seed", str(seed), "--alpha", str(alpha), "--json", out_json]
    if params_json:
        tail += ["--params", params_json]
    if direction:
        tail += ["--direction", direction]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    return tail


def m3_argv_tail(strategy, params_json, registry, direction, close_json,
                 windows, datasets, out_json) -> list:
    tail = ["--strategy", strategy, "--registry", registry,
            "--windows", _csv(windows), "--json", out_json]
    if params_json:
        tail += ["--params", params_json]
    if direction:
        tail += ["--direction", direction]
    if close_json:
        tail += ["--close-strategies", close_json]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    return tail


def m5_argv_tail(strategy, registry, direction, windows, datasets, out_json) -> list:
    tail = ["--strategies", strategy, "--registry", registry,
            "--windows", _csv(windows), "--json", out_json]
    if direction:
        tail += ["--direction", direction]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    return tail


def mc_argv_tail(candidate_path, registry, windows, datasets, n_paths, seed,
                 mc: dict, out_json) -> list:
    """Advisory Monte Carlo (#1295), multi-leg mode.

    Threads the CANDIDATE JSON — not a bare --strategy/--params pair — so the
    resampled trade series carries the candidate's close stack, entry gate and
    stops. A bare-strategy tail would resample a strategy nobody is ranking.
    """
    tail = ["--candidate-json", candidate_path, "--registry", registry,
            "--windows", _csv(windows), "--n-paths", str(n_paths),
            "--seed", str(seed), "--json", out_json]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    mc = mc or {}
    if mc.get("config"):
        tail += ["--config", mc["config"], "--strategy-id", mc["strategy_id"]]
    elif mc.get("kill_switch_pct") is not None:
        tail += ["--kill-switch-pct", str(mc["kill_switch_pct"])]
    return tail


def m6_argv_tail(m6_candidate, registry, windows, datasets, resamples, seed, out_json) -> list:
    tail = ["--strategy", m6_candidate["strategy_id"],
            "--registry", registry,
            "--candidate-close", json.dumps(m6_candidate["candidate_close"]),
            "--candidate-stops", m6_candidate.get("candidate_stops", "inherit"),
            "--windows", _csv(windows),
            "--bootstrap-resamples", str(resamples),
            "--seed", str(seed), "--json", out_json]
    # Exactly one incumbent source (load_spec enforces this): a live-daemon
    # config to resolve the strategy's live close, or an explicit ladder.
    if m6_candidate.get("baseline_config"):
        tail += ["--baseline-config", m6_candidate["baseline_config"]]
    elif m6_candidate.get("incumbent_close"):
        tail += ["--incumbent-close", json.dumps(m6_candidate["incumbent_close"])]
    for label in m6_candidate.get("allowed_regimes") or []:
        tail += ["--allowed-regimes", label]
    if datasets:
        tail += ["--datasets", _csv(datasets)]
    return tail


# ===========================================================================
# Pure — payload extractors (None-tolerant, mirror the harness JSON contracts)
# ===========================================================================

def extract_m1(payload: dict) -> dict:
    """window_scores -> {window: {verdict, mean_sharpe, mean_ddadj}}."""
    out = {}
    for score in (payload.get("window_scores") or []):
        w = score.get("window")
        if w is None:
            continue
        out[w] = {
            "verdict": score.get("verdict"),
            "mean_sharpe": score.get("mean_sharpe"),
            "mean_ddadj": score.get("mean_ddadj"),
        }
    return out


def extract_noise(payload: dict) -> dict:
    """gross_edge_noise trade_level -> {verdict, permutation_p, mean, n}."""
    tl = payload.get("trade_level") or {}
    perm = tl.get("permutation") or {}
    summary = tl.get("summary") or {}
    return {
        "verdict": tl.get("verdict"),
        "permutation_p": perm.get("p_value"),
        "mean": perm.get("mean"),
        "n": summary.get("n"),
    }


def m6_window_rollup(payload: dict) -> dict:
    """Generalized ``regime_1152._window_rollup``: paired-N-weighted pooled
    Δnet/entry per window PLUS the raw per-dataset (mean, n, p) evidence the
    family correction needs. Same None-guards as the 1152 driver."""
    out = {}
    for wname, results in (payload.get("results") or {}).items():
        deltas, per_dataset = [], []
        votes_pos = votes_neg = n_paired = 0
        for d in results or []:
            t = d.get("per_regime")
            if not t or t.get("all", {}).get("paired_delta", {}).get("mean") is None:
                continue
            mean = t["all"]["paired_delta"]["mean"]
            n = t["all"]["n"]
            p = t["all"]["paired_delta"].get("signed_rank", {}).get("p_value")
            deltas.append((mean, n))
            n_paired += n
            per_dataset.append({"dataset": d.get("dataset"), "mean": mean, "n": n, "p": p})
            if mean > 0:
                votes_pos += 1
            elif mean < 0:
                votes_neg += 1
        total_n = sum(n for _, n in deltas)
        pooled = round(sum(m * n for m, n in deltas) / total_n, 4) if total_n else None
        out[wname] = {
            "paired_n": n_paired,
            "pooled_delta_net_pct_per_entry": pooled,
            "datasets_delta_pos": votes_pos,
            "datasets_delta_neg": votes_neg,
            "per_dataset": per_dataset,
        }
    return out


def extract_m3(payload: dict) -> dict:
    """Compact per-window/per-dataset bleed-mode + fee-churn context (no p)."""
    out = {}
    for wname, per_ds in (payload.get("windows") or {}).items():
        out[wname] = {}
        for ds, diag in (per_ds or {}).items():
            if not diag:
                continue
            out[wname][ds] = {
                "bleed_modes": diag.get("bleed_modes"),
                "fee_churn": diag.get("fee_churn"),
            }
    return out


def _p95_max_dd(block: dict):
    """P95 max drawdown from a monte_carlo scheme block, when the run used the
    default percentile set. None — never a fabricated number — otherwise."""
    return (block.get("max_dd_pct_percentiles") or {}).get("p95")


def _worst(values: list):
    """Max over the non-None values; None when every leg is unusable. A risk
    column aggregates across datasets by WORST case — averaging would let one
    benign dataset mask a fragile one."""
    present = [v for v in values if v is not None]
    return max(present) if present else None


def extract_mc(payload: dict) -> dict:
    """monte_carlo multi-leg payload -> {window: {per_dataset, worst}}.

    ADVISORY ONLY (#1295): emits no p-value, and is never read by
    ``candidate_verdict`` or ``collect_family_pvalues``. ``worst`` carries the
    across-dataset worst case per scheme, which is what the shortlist prints.
    """
    out = {}
    for leg in (payload.get("legs") or []):
        w = leg.get("window")
        if w is None:
            continue
        bucket = out.setdefault(w, {"per_dataset": {}, "worst": {}})
        schemes = {}
        for b in (leg.get("schemes") or []):
            schemes[b.get("scheme")] = {
                "p_dd_ge_kill_switch": b.get("p_dd_ge_kill_switch"),
                "p95_max_dd": _p95_max_dd(b),
                "p_final_below_start": b.get("p_final_below_start"),
            }
        bucket["per_dataset"][leg.get("dataset")] = {
            "status": leg.get("status"),
            "n_trades": leg.get("n_trades"),
            "schemes": schemes,
        }
    for bucket in out.values():
        per_ds = list(bucket["per_dataset"].values())
        scheme_names = {s for d in per_ds for s in (d["schemes"] or {})}
        for scheme in sorted(scheme_names):
            rows = [d["schemes"].get(scheme) or {} for d in per_ds]
            bucket["worst"][scheme] = {
                k: _worst([r.get(k) for r in rows])
                for k in ("p_dd_ge_kill_switch", "p95_max_dd",
                          "p_final_below_start")
            }
    return out


def extract_m5(payload: dict, strategy: str) -> dict:
    """fee_audit rows -> the salvage screen for one strategy (no p)."""
    for row in (payload.get("rows") or []):
        if row.get("strategy") == strategy:
            return {
                "salvage_verdict": row.get("verdict"),
                "fee_drag_pp": row.get("fee_drag_pp"),
                "trades_per_year": row.get("trades_per_year"),
                "mean_gross_ret": row.get("mean_gross_ret"),
                "mean_net_ret": row.get("mean_net_ret"),
            }
    return {}


# ===========================================================================
# Pure — family correction, promotion gate, ranking
# ===========================================================================

def collect_family_pvalues(entries: list) -> list:
    """The pre-registered candidate family = every PRIMARY p-value produced by
    one full-spec invocation: one M1 step-2 permutation p per unique noise run,
    plus one M6 paired Wilcoxon p per (candidate, window, dataset). M3/M5 emit no
    p-values and contribute nothing here by construction."""
    tests = []
    seen_noise = set()
    for e in entries:
        r = e.get("results") or {}
        noise = (r.get("m1_noise") or {}).get("data")
        if noise is not None:
            fam = e.get("noise_family_key")
            if fam not in seen_noise:
                seen_noise.add(fam)
                p = noise.get("permutation_p")
                if p is not None:
                    # The noise p is deduped to ONE test per family (keeping the
                    # BH family size honest — the same noise run must not be
                    # counted once per sibling), but it is the shared evidence for
                    # EVERY entry in the family, so it is keyed by noise_family_key
                    # and looked up by family in candidate_verdict — not by the
                    # first sibling's candidate_key (which would let the other
                    # siblings skip the BH downgrade and promote on a failed p).
                    tests.append({"candidate_key": e["key"], "harness": "m1_noise",
                                  "noise_family_key": fam,
                                  "window": None, "dataset": None, "p": float(p),
                                  "effect_positive": (noise.get("mean") or 0) > 0})
        m6 = (r.get("m6") or {}).get("data")
        if m6 is not None:
            for wname, roll in sorted(m6.items()):
                for d in roll.get("per_dataset") or []:
                    if d.get("p") is None:
                        continue
                    tests.append({"candidate_key": e["key"], "harness": "m6",
                                  "window": wname, "dataset": d.get("dataset"),
                                  "p": float(d["p"]),
                                  "effect_positive": (d.get("mean") or 0) > 0})
    return tests


def apply_family_correction(tests: list, alpha: float = 0.05) -> dict:
    """One Benjamini-Hochberg pass over the whole family (the #1076 precedent).
    Stamps ``bh_pass`` onto each test and returns the correction summary
    including the effective threshold (largest passing p)."""
    pvals = [t["p"] for t in tests]
    mask = benjamini_hochberg(pvals, alpha)
    for t, passed in zip(tests, mask):
        t["bh_pass"] = bool(passed)
    passing = [t["p"] for t, ok in zip(tests, mask) if ok]
    m = len(tests)
    return {
        "method": "benjamini_hochberg",
        "alpha": alpha,
        "m": m,
        "effective_threshold": (max(passing) if passing else None),
        "bonferroni_threshold": (alpha / m if m else None),
        "n_survivors": sum(mask),
    }


def _tests_for(entry, tests: list) -> list:
    return [t for t in tests if t["candidate_key"] == entry["key"]]


def gate_relevant_results(entry: dict) -> dict:
    """The entry's harness runs MINUS the advisory ones (#1295).

    The only place the promotion gate is allowed to look at ``entry["results"]``.
    An advisory harness must not change a verdict by succeeding, by failing, or
    by being absent — filtering here is what makes that true, since the
    failed-run scan below keys on the dict's VALUES, not on any harness name.
    """
    return {h: r for h, r in (entry.get("results") or {}).items()
            if h not in ADVISORY_HARNESSES}


def any_gate_failure(entries: list) -> bool:
    """Did any GATE-relevant harness run fail? Drives the process exit code."""
    return any((v or {}).get("status") == "failed"
               for e in entries for v in gate_relevant_results(e).values())


def advisory_failures(entry: dict) -> list:
    """Advisory harnesses whose run failed — surfaced as a limitation, never as
    a verdict. Silence would be worse: an operator reads a blank MC column as
    "low risk" rather than "not measured"."""
    return sorted(h for h, r in (entry.get("results") or {}).items()
                  if h in ADVISORY_HARNESSES
                  and (r or {}).get("status") == "failed")


def candidate_verdict(entry: dict, tests: list) -> str:
    """Pre-registered promotion gate, generalized from ``regime_1152._verdict``
    with the BH layer added. A candidate is only a ``survivor`` when its positive
    evidence survives the family correction; positive-but-only-uncorrected is
    reported as its own verdict, never as a survivor."""
    if entry.get("precondition_errors"):
        return "excluded_not_replayable"
    # Advisory harnesses are filtered OUT before the failed-run scan, and are
    # never read below — the gate is byte-for-byte what it was pre-#1295.
    r = gate_relevant_results(entry)
    if any((v or {}).get("status") == "failed" for v in r.values()):
        return "run_failed"

    my_tests = _tests_for(entry, tests)
    # The noise p is deduped to one family-keyed test (see collect_family_pvalues),
    # so it lives under the FIRST sibling's candidate_key, not this entry's. Match
    # it across the whole family by noise_family_key so every sibling sharing the
    # family gets the same BH-survival verdict.
    fam = entry.get("noise_family_key")
    noise_t = next((t for t in tests if t["harness"] == "m1_noise"
                    and t.get("noise_family_key") == fam), None)

    if entry["kind"] == "open":
        noise = (r.get("m1_noise") or {}).get("data")
        if noise is not None and noise.get("verdict") == "no_positive_edge":
            return "noise_gate_blocked"
        m1 = (r.get("m1") or {}).get("data")
        if m1 is None:
            # No selectivity bar ran (m1 not requested): fall back to the noise
            # gate alone when present.
            if noise is None:
                return "inconclusive"
            if noise.get("verdict") == "distinguishable_positive":
                if noise_t and not noise_t.get("bh_pass"):
                    return "positive_uncorrected_only"
                return "survivor"
            return "incumbent_stands"
        rollup = m1  # already the extract_m1 rollup ({window: {verdict, ...}})
        protocol = [w for w in ("is", "oos") if w in rollup]
        if not protocol:
            return "inconclusive"
        if not all(rollup[w].get("verdict") == "pass" for w in protocol):
            return "incumbent_stands"
        # M1 protocol passed. If a noise p is in the family, gate the survivor
        # call on it surviving the correction.
        if noise_t and not noise_t.get("bh_pass"):
            return "positive_uncorrected_only"
        return "survivor"

    # exit_ab (M6)
    m6 = (r.get("m6") or {}).get("data")
    if m6 is None:
        return "inconclusive"
    is_w, oos_w = m6.get("is") or {}, m6.get("oos") or {}
    pooled_is = is_w.get("pooled_delta_net_pct_per_entry")
    pooled_oos = oos_w.get("pooled_delta_net_pct_per_entry")
    if pooled_is is None or pooled_oos is None:
        return "inconclusive"
    if not (pooled_is > 0 and pooled_oos > 0):
        return "incumbent_stands"
    pos = [t for t in my_tests if t["harness"] == "m6" and t["effect_positive"]]
    neg = [t for t in my_tests if t["harness"] == "m6" and not t["effect_positive"]]
    # Contradictions block at RAW p<0.05 (uncorrected — deliberately conservative,
    # matching the 1152 gate: a significant loss anywhere kills the candidate).
    if any(t["p"] < 0.05 for t in neg):
        return "incumbent_stands"
    if any(t.get("bh_pass") for t in pos):
        return "survivor"
    if any(t["p"] < 0.05 for t in pos):
        return "positive_uncorrected_only"
    return "positive_but_not_significant"


def _rank_score(entry: dict) -> float:
    """Sort key within the survivor/positive tiers: pooled OOS Δ/entry for M6,
    mean OOS sharpe for open candidates (higher = better)."""
    r = entry.get("results") or {}
    m6 = (r.get("m6") or {}).get("data")
    if m6:
        oos = (m6.get("oos") or {}).get("pooled_delta_net_pct_per_entry")
        return oos if oos is not None else float("-inf")
    m1 = (r.get("m1") or {}).get("data")
    if m1:
        s = m1.get("oos", {}).get("mean_sharpe")
        return s if s is not None else float("-inf")
    return float("-inf")


def rank_shortlist(entries: list) -> list:
    """Survivors first, then positive-uncorrected, then the rest by verdict
    order; failed/excluded runs stay visible (never silently dropped)."""
    order = {v: i for i, v in enumerate(VERDICT_ORDER)}
    return sorted(
        entries,
        key=lambda e: (order.get(e.get("verdict"), len(VERDICT_ORDER)),
                       -_rank_score(e), e["key"]),
    )


def reproduction_command(entry: dict) -> list:
    """Copy-pasteable per-harness reproduction commands (the fee_audit
    ``_reproduce_command`` precedent) plus the suggester's own --only line."""
    cmds = []
    for harness, run in (entry.get("results") or {}).items():
        tail = run.get("argv_tail")
        if not tail:
            continue
        rel = HARNESS_REL[harness]
        cmds.append("uv run --no-sync python " + rel + " "
                    + " ".join(shlex.quote(str(a)) for a in tail))
    return cmds


# ===========================================================================
# Pure — report formatting
# ===========================================================================

def _mc_segment(mc: dict) -> str:
    """The advisory MC column: worst-dataset sequencing risk, OOS if scored.

    Deliberately labeled ``(adv)`` and printed with the window it came from —
    an unlabeled probability next to a promotion verdict reads as evidence.
    """
    if not mc:
        return ""
    window = "oos" if "oos" in mc else sorted(mc)[0]
    stats = ((mc.get(window) or {}).get("worst") or {}).get(MC_SCHEME_FOR_COLUMNS)
    if not stats:
        return ""

    def _f(key, prec):
        v = stats.get(key)
        return "-" if v is None else format(v, f".{prec}f")

    return (f"  MC(adv,{window})=p95DD {_f('p95_max_dd', 1)}% "
            f"pKS {_f('p_dd_ge_kill_switch', 3)} "
            f"pDown {_f('p_final_below_start', 3)}")


def format_shortlist(report: dict) -> str:
    corr = report["correction"]
    lines = [f"== auto-suggest shortlist: {report['study']} =="]
    if report.get("exploratory"):
        lines.append("*** EXPLORATORY — correction family incomplete "
                     f"(ran {report['ran']} of {report['total']} candidates; "
                     "the committed artifact must come from a full-spec run) ***")
    thr = corr.get("effective_threshold")
    lines.append(
        f"correction: {corr['method']} alpha={corr['alpha']} over m={corr['m']} "
        f"pooled p-values; effective threshold "
        f"{('p<=' + format(thr, '.4g')) if thr is not None else 'none pass'} "
        f"(Bonferroni {format(corr['bonferroni_threshold'], '.4g') if corr['bonferroni_threshold'] else 'n/a'}); "
        f"{corr['n_survivors']} test(s) survive.")
    lines.append("")
    for i, e in enumerate(report["ranked"], 1):
        r = e.get("results") or {}
        extra = ""
        m6 = (r.get("m6") or {}).get("data")
        if m6:
            pis = (m6.get("is") or {}).get("pooled_delta_net_pct_per_entry")
            poos = (m6.get("oos") or {}).get("pooled_delta_net_pct_per_entry")
            extra = f"  pooledΔ/e is={pis} oos={poos}"
        m1 = (r.get("m1") or {}).get("data")
        if m1:
            v = {w: s.get("verdict") for w, s in m1.items()}
            extra += f"  M1={v}"
        m5 = (r.get("m5") or {}).get("data")
        if m5:
            extra += f"  M5(ctx)={m5.get('salvage_verdict')}"
        extra += _mc_segment((r.get("mc") or {}).get("data"))
        limn = (" [" + ",".join(e["limitations"]) + "]") if e.get("limitations") else ""
        lines.append(f"{i:>2}  {e['key']:<40} {e['verdict']:<26}{extra}{limn}")
    lines.append("")
    lines.append("M3/M5/MC figures are UNCORRECTED CONTEXT (no p-values), never "
                 "counted as significance evidence.")
    lines.append(f"MC(adv) = trade-order Monte Carlo (#1274), "
                 f"{MC_SCHEME_FOR_COLUMNS} scheme, WORST dataset in the window: "
                 "p95DD = P95 max drawdown, pKS = P(max DD >= kill switch), "
                 "pDown = P(final < start). Advisory only — it does not gate "
                 "promotion, and a failed MC run leaves the verdict untouched "
                 "(flagged 'mc_run_failed'). Open candidates only; M6 exit-A/B "
                 "entries carry no MC column.")
    lines.append(FOOTER)
    return "\n".join(lines)


# ===========================================================================
# I/O — orchestration (modeled on regime_1152's _run_one / main)
# ===========================================================================

def _direction_for(candidate: dict):
    """Noise/M3/M5 accept only long/short; a 'both' candidate has no single
    open-side leg there, so we drop the flag (harness default = long)."""
    d = str(candidate.get("direction") or "").strip().lower()
    return d if d in ("long", "short") else None


def _noise_family_key(candidate: dict) -> str:
    return json.dumps({
        "name": candidate.get("name"),
        "params": candidate.get("params"),
        "direction": _direction_for(candidate),
    }, sort_keys=True)


def _m5_family_key(candidate: dict) -> str:
    # fee_audit screens registry defaults by name+direction (no --params surface),
    # so params are deliberately excluded — one m5 run covers a name+direction.
    return json.dumps({"name": candidate.get("name"),
                       "direction": _direction_for(candidate)}, sort_keys=True)


def _run_harness(harness: str, tail: list, out_json: str) -> dict:
    argv = [sys.executable, HARNESS_ABS[harness]] + tail
    proc = subprocess.run(argv, capture_output=True, text=True)
    ok = proc.returncode == 0 and os.path.exists(out_json)
    run = {"harness": harness, "argv_tail": tail, "status": "ok" if ok else "failed"}
    if ok:
        with open(out_json) as fh:
            run["payload"] = json.load(fh)
    else:
        sys.stderr.write(f"[{harness}] FAILED rc={proc.returncode}\n"
                         f"{proc.stdout[-1500:]}\n{proc.stderr[-1500:]}\n")
    return run


def ensure_noise(entry: dict, spec: dict, out_dir: str, noise_cache: dict) -> None:
    """Run (or reuse) the M1 step-2 noise gate for this entry's base open. The
    #1054 contract runs it BEFORE any selectivity work, so main() primes the
    cache with this in a serial pre-pass; per-base results are memoized."""
    cand = entry["candidate"]
    entry["noise_family_key"] = _noise_family_key(cand)
    if "m1_noise" not in entry["harnesses"]:
        return
    fam = entry["noise_family_key"]
    if fam not in noise_cache:
        out = os.path.join(out_dir, f"{entry['key']}.noise.json")
        tail = noise_argv_tail(
            cand["name"], json.dumps(cand["params"]) if cand.get("params") else None,
            spec["registry"], _direction_for(cand), spec["windows"], spec["datasets"],
            spec["resamples"], spec["seed"], spec["correction"]["alpha"], out)
        run = _run_harness("m1_noise", tail, out)
        if run["status"] == "ok":
            run["data"] = extract_noise(run.pop("payload"))
        noise_cache[fam] = run


def ensure_m5(entry: dict, spec: dict, out_dir: str, m5_cache: dict) -> None:
    """Run (or reuse) the family-level M5 fee audit. Like the noise gate, this
    MUST be primed serially before the parallel map — otherwise two entries
    sharing an m5 family race on the same fee_audit output path and a read can
    hit a half-written file. Per (name, direction) results are memoized."""
    cand = entry["candidate"]
    if "m5" not in entry["harnesses"]:
        return
    fam = _m5_family_key(cand)
    if fam not in m5_cache:
        direction = _direction_for(cand)
        out = os.path.join(out_dir, f"m5.{cand['name']}.{direction or 'long'}.json")
        tail = m5_argv_tail(cand["name"], spec["registry"], direction,
                            spec["windows"], spec["datasets"], out)
        run = _run_harness("m5", tail, out)
        if run["status"] == "ok":
            run["data"] = extract_m5(run.pop("payload"), cand["name"])
        m5_cache[fam] = run


def run_open_entry(entry: dict, spec: dict, out_dir: str,
                   noise_cache: dict, m5_cache: dict) -> dict:
    cand = entry["candidate"]
    reg, windows, datasets = spec["registry"], spec["windows"], spec["datasets"]
    key = entry["key"]
    results = {}

    ensure_noise(entry, spec, out_dir, noise_cache)
    if "m1_noise" in entry["harnesses"]:
        results["m1_noise"] = noise_cache[entry["noise_family_key"]]

    def candidate_path() -> str:
        # Shared by m1 and mc — both consume the SAME candidate JSON, so the
        # advisory resampler cannot drift onto a narrower view of the candidate.
        path = os.path.join(out_dir, f"{key}.candidate.json")
        if not os.path.exists(path):
            with open(path, "w") as fh:
                json.dump(cand, fh, indent=2)
        return path

    if "m1" in entry["harnesses"]:
        cand_path = candidate_path()
        out = os.path.join(out_dir, f"{key}.m1.json")
        run = _run_harness("m1", m1_argv_tail(cand_path, reg, windows, datasets, out), out)
        if run["status"] == "ok":
            run["data"] = extract_m1(run.pop("payload"))
        results["m1"] = run

    if "m3" in entry["harnesses"]:
        out = os.path.join(out_dir, f"{key}.m3.json")
        close_json = (json.dumps(cand["close_strategies"])
                      if cand.get("close_strategies") else None)
        tail = m3_argv_tail(cand["name"],
                            json.dumps(cand["params"]) if cand.get("params") else None,
                            reg, _direction_for(cand), close_json, windows, datasets, out)
        run = _run_harness("m3", tail, out)
        if run["status"] == "ok":
            run["data"] = extract_m3(run.pop("payload"))
        results["m3"] = run

    if "m5" in entry["harnesses"]:
        # m5 is family-level (fee_audit screens registry defaults by name, no
        # --params surface). It MUST be primed serially by ensure_m5 before the
        # parallel map — a check-then-run here would let two threads sharing an
        # m5 family both miss the cache, both spawn fee_audit writing the same
        # output path, and a read racing that rewrite would raise JSONDecodeError
        # and abort the whole suggester. By this point the cache is populated.
        results["m5"] = m5_cache[_m5_family_key(cand)]

    if "mc" in entry["harnesses"]:
        # ADVISORY (#1295) — per-candidate, unlike the family-keyed noise/m5
        # runs: the resampled trade series depends on this candidate's entry
        # gate, close stack and params, so two siblings sharing a noise family
        # still need their own Monte Carlo.
        out = os.path.join(out_dir, f"{key}.mc.json")
        mc = spec.get("mc") or {}
        tail = mc_argv_tail(candidate_path(), reg, windows, datasets,
                            mc.get("n_paths") or MC_DEFAULT_N_PATHS,
                            spec["seed"], mc, out)
        run = _run_harness("mc", tail, out)
        if run["status"] == "ok":
            run["data"] = extract_mc(run.pop("payload"))
        results["mc"] = run

    entry["results"] = results
    for h in advisory_failures(entry):
        entry["limitations"].append(f"{h}_run_failed")
    return entry


def run_exit_ab_entry(entry: dict, spec: dict, out_dir: str) -> dict:
    if entry["precondition_errors"]:
        entry["results"] = {}
        return entry
    out = os.path.join(out_dir, f"{entry['key']}.m6.json")
    tail = m6_argv_tail(entry["candidate"], spec["registry"], spec["windows"],
                        spec["datasets"], spec["resamples"], spec["seed"], out)
    run = _run_harness("m6", tail, out)
    if run["status"] == "ok":
        run["data"] = m6_window_rollup(run.pop("payload"))
    entry["results"] = {"m6": run}
    return entry


def _cmd(harness: str, tail: list) -> str:
    return ("uv run --no-sync python " + HARNESS_REL[harness] + " "
            + " ".join(shlex.quote(str(a)) for a in tail))


def _dry_run_commands(entries: list, spec: dict, out_dir: str) -> list:
    """Every planned command, without running anything.

    EVERY enabled harness appears here. A dry run that omits a harness it will
    actually spawn under-reports the plan — the one thing a dry run exists to
    prevent.
    """
    cmds = []
    for e in entries:
        reg, windows, datasets = spec["registry"], spec["windows"], spec["datasets"]
        if e["precondition_errors"]:
            cmds.append(f"# {e['key']}: SKIP ({','.join(e['precondition_errors'])})")
            continue
        if e["kind"] == "open":
            cand = e["candidate"]
            key, direction = e["key"], _direction_for(cand)
            params_json = json.dumps(cand["params"]) if cand.get("params") else None
            cand_path = os.path.join(out_dir, f"{key}.candidate.json")
            if "m1_noise" in e["harnesses"]:
                cmds.append(_cmd("m1_noise", noise_argv_tail(
                    cand["name"], params_json, reg, direction, windows, datasets,
                    spec["resamples"], spec["seed"], spec["correction"]["alpha"],
                    os.path.join(out_dir, f"{key}.noise.json"))))
            if "m1" in e["harnesses"]:
                cmds.append(_cmd("m1", m1_argv_tail(
                    cand_path, reg, windows, datasets,
                    os.path.join(out_dir, f"{key}.m1.json"))))
            if "m3" in e["harnesses"]:
                close_json = (json.dumps(cand["close_strategies"])
                              if cand.get("close_strategies") else None)
                cmds.append(_cmd("m3", m3_argv_tail(
                    cand["name"], params_json, reg, direction, close_json,
                    windows, datasets, os.path.join(out_dir, f"{key}.m3.json"))))
            if "m5" in e["harnesses"]:
                cmds.append(_cmd("m5", m5_argv_tail(
                    cand["name"], reg, direction, windows, datasets,
                    os.path.join(out_dir,
                                 f"m5.{cand['name']}.{direction or 'long'}.json"))))
            if "mc" in e["harnesses"]:
                mc = spec.get("mc") or {}
                cmds.append(_cmd("mc", mc_argv_tail(
                    cand_path, reg, windows, datasets,
                    mc.get("n_paths") or MC_DEFAULT_N_PATHS, spec["seed"], mc,
                    os.path.join(out_dir, f"{key}.mc.json"))))
        else:
            cmds.append(_cmd("m6", m6_argv_tail(
                e["candidate"], reg, windows, datasets,
                spec["resamples"], spec["seed"],
                os.path.join(out_dir, f"{e['key']}.m6.json"))))
    return cmds


def _serializable(entry: dict) -> dict:
    """Strip subprocess bookkeeping the artifact does not need."""
    out = {k: entry[k] for k in ("key", "kind", "hypothesis", "verdict",
                                 "limitations", "precondition_errors")}
    out["candidate"] = entry["candidate"]
    out["evidence"] = {h: {"status": r.get("status"), "data": r.get("data")}
                       for h, r in (entry.get("results") or {}).items()}
    out["reproduce"] = reproduction_command(entry)
    return out


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--spec", required=True, help="Path to a suggest.json spec")
    p.add_argument("--jobs", type=int, default=4)
    p.add_argument("--out-dir", default=None, help="Per-run harness JSON dir "
                   "(default: <spec_dir>/<study>_runs)")
    p.add_argument("--only", default=None, help="Comma list of candidate keys "
                   "(stamps the report EXPLORATORY — the correction is only valid "
                   "over the full family)")
    p.add_argument("--windows", default=None, help="Override spec windows")
    p.add_argument("--datasets", default=None, help="Override spec datasets "
                   "(comma list SYMBOL:TIMEFRAME)")
    p.add_argument("--alpha", type=float, default=None, help="Override correction alpha")
    p.add_argument("--seed", type=int, default=1066)
    p.add_argument("--bootstrap-resamples", type=int, default=10000)
    p.add_argument("--dry-run", action="store_true",
                   help="Print every planned harness command; run nothing")
    p.add_argument("--json", default=None, dest="json_out", help="Write the artifact")
    p.add_argument("--markdown", default=None, dest="markdown_out")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)

    spec_path = os.path.abspath(args.spec)
    with open(spec_path) as fh:
        raw = json.load(fh)
    spec = load_spec(raw, os.path.dirname(spec_path))

    if args.windows:
        spec["windows"] = [w.strip() for w in args.windows.split(",") if w.strip()]
        bad = [w for w in spec["windows"] if w not in M1_WINDOWS]
        if bad:
            raise SystemExit(f"unknown windows {bad}; known: {list(M1_WINDOWS)}")
    if args.datasets is not None:
        spec["datasets"] = [d.strip() for d in args.datasets.split(",") if d.strip()]
    if args.alpha is not None:
        spec["correction"]["alpha"] = args.alpha
    spec["seed"] = args.seed
    spec["resamples"] = args.bootstrap_resamples

    entries = expand_candidates(spec)
    total = len(entries)
    exploratory = False
    if args.only:
        keys = {k.strip() for k in args.only.split(",") if k.strip()}
        unknown = keys - {e["key"] for e in entries}
        if unknown:
            raise SystemExit(f"unknown candidate keys: {sorted(unknown)}")
        entries = [e for e in entries if e["key"] in keys]
        exploratory = len(entries) < total

    out_dir = args.out_dir or os.path.join(spec["spec_dir"], f"{spec['study']}_runs")

    if args.dry_run:
        for cmd in _dry_run_commands(entries, spec, out_dir):
            print(cmd)
        print("\n# dry-run: nothing executed. " + FOOTER)
        return 0

    os.makedirs(out_dir, exist_ok=True)
    noise_cache, m5_cache = {}, {}
    open_entries = [e for e in entries if e["kind"] == "open"]
    ab_entries = [e for e in entries if e["kind"] == "exit_ab"]

    # Noise gate FIRST (M1 step-2 precondition), serialized so the family cache
    # is populated before selectivity work; the family-level M5 audit is primed
    # in the same serial pass so concurrent entries never race its output path.
    for e in open_entries:
        ensure_noise(e, spec, out_dir, noise_cache)
        ensure_m5(e, spec, out_dir, m5_cache)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as ex:
        list(ex.map(lambda e: run_open_entry(e, spec, out_dir, noise_cache, m5_cache),
                    open_entries))
        list(ex.map(lambda e: run_exit_ab_entry(e, spec, out_dir), ab_entries))

    tests = collect_family_pvalues(entries)
    correction = apply_family_correction(tests, spec["correction"]["alpha"])
    for e in entries:
        e["verdict"] = candidate_verdict(e, tests)
    ranked = rank_shortlist(entries)

    report = {
        "study": spec["study"],
        "issue": 1210,
        "registry": spec["registry"],
        "windows": spec["windows"],
        "correction": correction,
        "family_tests": tests,
        "exploratory": exploratory,
        "ran": len(entries),
        "total": total,
        "ranked": [_serializable(e) for e in ranked],
        "note": FOOTER,
    }

    text = format_shortlist({**report, "ranked": ranked})
    print(text)

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(report, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    if args.markdown_out:
        with open(args.markdown_out, "w") as fh:
            fh.write("```\n" + text + "\n```\n")
        print(f"wrote {args.markdown_out}")

    # Advisory-harness failures are reported (stderr + a per-entry limitation
    # flag + a missing MC column) but never fail the run: an unavailable Monte
    # Carlo column must not change the process's success signal any more than
    # it changes a verdict (#1295).
    return 1 if any_gate_failure(entries) else 0


if __name__ == "__main__":
    raise SystemExit(main())
