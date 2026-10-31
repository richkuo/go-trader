from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
_ROOT = os.path.abspath(os.path.join(_THIS_DIR, ".."))
for _p in (_ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
from regime_stats import kruskal_h, benjamini_hochberg

DEFAULT_PERIOD = 48


def forward_returns(close: np.ndarray, horizon: int) -> np.ndarray:
    close = np.asarray(close, dtype=float)
    fwd = np.full(len(close), np.nan)
    if horizon < len(close):
        fwd[:-horizon] = close[horizon:] / close[:-horizon] - 1.0
    return fwd


def forward_realized_vol(close: np.ndarray, horizon: int) -> np.ndarray:
    close = np.asarray(close, dtype=float)
    n = len(close)
    out = np.full(n, np.nan)
    h = int(horizon)
    if n < 2 or h < 1 or h >= n:
        return out
    log_ret = np.diff(np.log(close), prepend=np.log(close[0]))
    for i in range(n - h):
        out[i] = np.sqrt(np.sum(log_ret[i + 1 : i + 1 + h] ** 2))
    return out


FORWARD_TARGETS = {"returns": forward_returns, "volatility": forward_realized_vol}


def coverage(labels: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    n = len(labels)
    states, counts = np.unique(labels, return_counts=True)
    return {str(s): {"count": int(c), "pct": float(c / n) if n else 0.0}
            for s, c in zip(states, counts)}


def separation(labels: np.ndarray, fwd: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    valid = ~np.isnan(fwd)
    labels, fwd = labels[valid], fwd[valid]
    per_state, groups = {}, []
    for s in sorted(set(labels.tolist())):
        r = fwd[labels == s]
        if len(r) == 0:
            continue
        groups.append(r)
        per_state[str(s)] = {
            "n": int(len(r)),
            "mean": float(r.mean()),
            "std": float(r.std()),
            "hit_rate": float((r > 0).mean()),
        }
    return {"kruskal_h": kruskal_h(groups), "per_state": per_state}


def stability(labels: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    n = len(labels)
    valid = np.array([bool(str(v or "").strip()) for v in labels])
    adjacent = valid[:-1] & valid[1:]
    flips = int(((labels[1:] != labels[:-1]) & adjacent).sum())
    runs: dict[str, list[int]] = {}
    cur, length = "", 0
    for x, supported in zip(labels, valid):
        if supported and x == cur:
            length += 1
        else:
            if length:
                runs.setdefault(str(cur), []).append(length)
            cur, length = (x, 1) if supported else ("", 0)
    if length:
        runs.setdefault(str(cur), []).append(length)
    return {"transition_rate": float(flips / adjacent.sum()) if adjacent.any() else 0.0,
            "flips": flips, "mean_dwell": {s: float(np.mean(v)) for s, v in sorted(runs.items())}}


def block_shuffle_pvalue(labels, fwd, block_len, n_perm=200, seed=0) -> dict:
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    h_obs = separation(labels, fwd)["kruskal_h"]
    n = len(labels)
    block_len = max(1, int(block_len))
    starts = list(range(0, n, block_len))
    rng = np.random.default_rng(seed)
    ge = 0
    for _ in range(n_perm):
        perm = rng.permutation(len(starts))
        shuffled = np.concatenate([labels[b : b + block_len] for b in (starts[i] for i in perm)])
        shuffled = shuffled[:n]
        if separation(shuffled, fwd)["kruskal_h"] >= h_obs:
            ge += 1
    return {"kruskal_h": float(h_obs), "p_value": float((ge + 1) / (n_perm + 1)),
            "block_len": block_len, "n_perm": int(n_perm)}


def per_state_significance(labels, fwd, block_len, n_perm=200, seed=0) -> dict:
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    valid = ~np.isnan(fwd)
    labels, fwd = labels[valid], fwd[valid]
    states = sorted(set(labels.tolist()))

    gap_obs: dict[str, float] = {}
    for s in states:
        in_group = fwd[labels == s]
        out_group = fwd[labels != s]
        if len(in_group) == 0 or len(out_group) == 0:
            continue
        gap_obs[s] = float(in_group.mean() - out_group.mean())

    if not gap_obs:
        return {}

    active_states = sorted(gap_obs.keys())
    ge: dict[str, int] = {s: 0 for s in active_states}

    n = len(labels)
    block_len = max(1, int(block_len))
    starts = list(range(0, n, block_len))
    rng = np.random.default_rng(seed)

    for _ in range(n_perm):
        perm = rng.permutation(len(starts))
        shuffled = np.concatenate([labels[b : b + block_len] for b in (starts[i] for i in perm)])
        shuffled = shuffled[:n]
        for s in active_states:
            in_perm = fwd[shuffled == s]
            out_perm = fwd[shuffled != s]
            if len(in_perm) == 0 or len(out_perm) == 0:
                continue
            gap_perm = float(in_perm.mean() - out_perm.mean())
            if abs(gap_perm) >= abs(gap_obs[s]):
                ge[s] += 1

    p_vals = {s: float((ge[s] + 1) / (n_perm + 1)) for s in active_states}
    bh_results = benjamini_hochberg([p_vals[s] for s in active_states])
    return {
        s: {"gap": gap_obs[s], "p_value": p_vals[s], "fdr_reject": bool(reject)}
        for s, reject in zip(active_states, bh_results)
    }


def _kmeans(z, k, seed, iters=50):
    rng = np.random.default_rng(seed)
    centers = z[rng.choice(len(z), size=k, replace=False)]
    labels = np.zeros(len(z), dtype=int)
    for _ in range(iters):
        d = ((z[:, None, :] - centers[None, :, :]) ** 2).sum(-1)
        new = d.argmin(1)
        if np.array_equal(new, labels):
            break
        labels = new
        for j in range(k):
            if (labels == j).any():
                centers[j] = z[labels == j].mean(0)
    return labels


def kmeans_yardstick(features, fwd, k_range=(2, 3, 4, 5, 6, 7), seed=0) -> dict:
    features = np.asarray(features, dtype=float)
    fwd = np.asarray(fwd, dtype=float)
    mask = ~np.isnan(features).any(1) & ~np.isnan(fwd)
    x, fr = features[mask], fwd[mask]
    mean, std = x.mean(0), x.std(0)
    std[std < 1e-8] = 1.0
    z = (x - mean) / std
    out = {}
    for k in k_range:
        if k > len(z):
            continue
        cl = _kmeans(z, k, seed)
        groups = [fr[cl == j] for j in range(k) if (cl == j).any()]
        out[k] = {"kruskal_h": kruskal_h(groups)}
    return out


def score_labels(close, labels, features, horizons=(1, 4, 12), block_mult=3, seed=0,
                 target="returns", n_perm=200) -> dict:
    labels = np.asarray(labels, dtype=object)
    features = np.asarray(features, dtype=float)
    try:
        fwd_fn = FORWARD_TARGETS[target]
    except KeyError:
        raise ValueError(f"unknown forward target {target!r}; known: {sorted(FORWARD_TARGETS)}")
    valid = np.isfinite(features).all(axis=1) & np.array([bool(str(v or "").strip()) for v in labels])
    vlabels = labels[valid]
    st = stability(np.where(valid, labels, ""))
    mean_dwell = float(np.mean(list(st["mean_dwell"].values()))) if st["mean_dwell"] else 1.0
    out = {"target": target, "coverage": coverage(vlabels), "stability": st, "horizons": {}}
    if not valid.any():
        return {**out, "status": "unavailable", "reason": "no ready regime samples"}
    for h in horizons:
        fwd_full = fwd_fn(close, h)
        fwd = fwd_full[valid]
        block_len = max(int(block_mult * mean_dwell), h)
        out["horizons"][f"h{h}"] = {
            "separation": separation(vlabels, fwd),
            "significance": block_shuffle_pvalue(vlabels, fwd, block_len, n_perm=n_perm,
                                                 seed=seed),
            "yardstick": kmeans_yardstick(features, fwd_full, seed=seed),
            "per_state_fdr": per_state_significance(vlabels, fwd, block_len, n_perm=n_perm,
                                                    seed=seed),
        }
    if "h4" in out["horizons"]:
        out["h4"] = out["horizons"]["h4"]
    return out


_OMITTED = object()


def _finite_number(name, value) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a finite number, got {value!r}") from exc
    if number != number or number in (float("inf"), float("-inf")):
        raise ValueError(f"{name} must be a finite number, got {value!r}")
    return number


def _parse_adx_period_cap(value):
    if value is _OMITTED:
        return _OMITTED
    if value is None or (isinstance(value, str) and value.strip().lower() == "none"):
        return None
    if isinstance(value, str):
        text = value.strip()
        if not text or not text.lstrip("-").isdigit():
            raise ValueError(f"adx_period_cap must be a positive int or 'none', got {value!r}")
        value = int(text)
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise ValueError(f"adx_period_cap must be a positive int or 'none', got {value!r}")
    return value


def _composite_threshold_overrides(return_eff, range_eff, efficiency, composite_adx_threshold):
    raw = {}
    if return_eff is not None:
        raw["return_eff"] = _finite_number("return_eff", return_eff)
    if range_eff is not None:
        raw["range_eff"] = _finite_number("range_eff", range_eff)
    if efficiency is not None:
        raw["efficiency"] = _finite_number("efficiency", efficiency)
    if composite_adx_threshold is not None:
        raw["adx"] = _finite_number("composite_adx_threshold", composite_adx_threshold)
    return raw


def _classifier_inputs_explicit(classifier, period, adx_threshold, return_eff, range_eff,
                                efficiency, composite_adx_threshold, adx_period_cap) -> bool:
    return not (
        classifier is None and period is None and adx_threshold is None
        and return_eff is None and range_eff is None and efficiency is None
        and composite_adx_threshold is None and adx_period_cap is _OMITTED
    )


def _resolve_exchange(exchange):
    from eval_windows import PLATFORM
    if exchange is None:
        return PLATFORM
    text = str(exchange).strip()
    if not text:
        raise ValueError("exchange must be a non-empty string")
    return text


def _validate_period(period):
    if period is None:
        return None
    if isinstance(period, bool) or not isinstance(period, int) or period < 2:
        raise ValueError(f"period must be an int >= 2, got {period!r}")
    return period


def run_window(symbol, timeframe, window, *, model=None, horizons=(1, 4, 12), seed=0,
               target="returns", n_perm=200, classifier=None, period=None,
               adx_threshold=None, return_eff=None, range_eff=None, efficiency=None,
               composite_adx_threshold=None, adx_period_cap=_OMITTED, exchange=None) -> dict:
    from regime import (compute_regime, compute_regime_composite, composite_feature_matrix,
                        _DEFAULT_COMPOSITE_THRESHOLDS, COMPOSITE_ADX_PERIOD_CAP,
                        normalize_composite_thresholds)
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS
    period = _validate_period(period)
    if adx_threshold is not None:
        adx_threshold = _finite_number("adx_threshold", adx_threshold)
    cap = _parse_adx_period_cap(adx_period_cap)
    threshold_overrides = _composite_threshold_overrides(
        return_eff, range_eff, efficiency, composite_adx_threshold)
    explicit = _classifier_inputs_explicit(
        classifier, period, adx_threshold, return_eff, range_eff, efficiency,
        composite_adx_threshold, adx_period_cap)
    if classifier is not None and classifier not in ("adx", "composite"):
        raise ValueError(f"classifier must be 'adx' or 'composite', got {classifier!r}")
    if model is not None and explicit:
        raise ValueError("conflicting model and classifier overrides")
    if classifier == "adx" and threshold_overrides:
        raise ValueError("composite thresholds conflict with classifier 'adx'")
    if classifier == "adx" and cap is not _OMITTED:
        raise ValueError("adx_period_cap conflicts with classifier 'adx'")
    if (classifier == "composite" and adx_threshold is not None
            and composite_adx_threshold is not None
            and float(adx_threshold) != float(composite_adx_threshold)):
        raise ValueError("conflicting ADX thresholds")
    exchange_id = _resolve_exchange(exchange)
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=exchange_id, start_date=start, end_date=end)
    if not explicit:
        period_used = int(model["period"]) if model and "period" in model else DEFAULT_PERIOD
        th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
        feats_df = composite_feature_matrix(df, period_used, th)
        features = feats_df.to_numpy()
        if model is None:
            labels = compute_regime_composite(df, period=period_used, thresholds=th)["regime"].to_numpy()
        else:
            from regime_hmm import forward_filter_labels
            labels, _conf = forward_filter_labels(features, model)
        return score_labels(df["close"].to_numpy(), labels, features, horizons=horizons,
                            seed=seed, target=target, n_perm=n_perm)

    hand_classifier = classifier or "composite"
    if hand_classifier == "adx":
        period_used = 14 if period is None else period
        threshold_used = 20.0 if adx_threshold is None else float(adx_threshold)
        reg = compute_regime(df, period=period_used, adx_threshold=threshold_used)
        labels = reg["regime"].to_numpy()
        features = np.column_stack([
            reg["adx"].to_numpy(dtype=float),
            reg["plus_di"].to_numpy(dtype=float),
            reg["minus_di"].to_numpy(dtype=float),
        ])
        ready = reg["regime_ready"].to_numpy(dtype=bool)
        features[~ready] = np.nan
    else:
        period_used = DEFAULT_PERIOD if period is None else period
        th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
        if adx_threshold is not None and "adx" not in threshold_overrides:
            threshold_overrides = dict(threshold_overrides)
            threshold_overrides["adx"] = float(adx_threshold)
        if threshold_overrides:
            th.update(normalize_composite_thresholds(threshold_overrides, with_defaults=False))
        cap_used = COMPOSITE_ADX_PERIOD_CAP if cap is _OMITTED else cap
        feats_df = composite_feature_matrix(df, period_used, th, adx_period_cap=cap_used)
        features = feats_df.to_numpy()
        labels = compute_regime_composite(
            df, period=period_used, thresholds=th, adx_period_cap=cap_used)["regime"].to_numpy()
    return score_labels(df["close"].to_numpy(), labels, features, horizons=horizons,
                        seed=seed, target=target, n_perm=n_perm)


def build_parser() -> "argparse.ArgumentParser":
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(description="7-state regime quality diagnostics (#1065)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--windows", default="is,oos", help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--model-json", default=None, help="score a fitted model instead of the hand-rule")
    p.add_argument("--horizons", default="1,4,12")
    p.add_argument("--target", default="returns", choices=("returns", "volatility"),
                   help="forward variable the separation is scored on (default: returns)")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write report JSON to this path")
    p.add_argument("--classifier", default=None, choices=("adx", "composite"),
                   help="hand-rule classifier; omitted keeps the composite hand-rule")
    p.add_argument("--period", type=int, default=None,
                   help="classifier period; omitted keeps the composite or fitted-model period rule")
    p.add_argument("--adx-threshold", type=float, default=None,
                   help="ADX label threshold, or the composite ADX threshold when classifier=composite")
    p.add_argument("--return-eff", type=float, default=None)
    p.add_argument("--range-eff", type=float, default=None)
    p.add_argument("--efficiency", type=float, default=None)
    p.add_argument("--composite-adx-threshold", type=float, default=None)
    p.add_argument("--adx-period-cap", default=None,
                   help="composite ADX cap, or 'none' for uncapped; omitted keeps the shipped cap")
    p.add_argument("--exchange", default=None, help="candle exchange id; omitted keeps binanceus")
    return p


def _cli_inputs(args) -> dict:
    cap = None if args.adx_period_cap is None else args.adx_period_cap
    return {
        "classifier": args.classifier,
        "period": args.period,
        "adx_threshold": args.adx_threshold,
        "return_eff": args.return_eff,
        "range_eff": args.range_eff,
        "efficiency": args.efficiency,
        "composite_adx_threshold": args.composite_adx_threshold,
        "adx_period_cap": cap,
        "exchange": args.exchange,
    }


def main(argv=None) -> int:
    import json
    args = build_parser().parse_args(argv)
    from eval_windows import WINDOWS
    model = None
    if args.model_json:
        with open(args.model_json) as fh:
            loaded = json.load(fh)
        model = loaded.get("model", loaded) if isinstance(loaded, dict) else loaded
    horizons = tuple(int(x) for x in args.horizons.split(","))
    cap = _OMITTED if args.adx_period_cap is None else args.adx_period_cap
    report = {}
    for w in args.windows.split(","):
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
        report[w] = run_window(
            args.symbol, args.timeframe, w, model=model, horizons=horizons, seed=args.seed,
            target=args.target, classifier=args.classifier, period=args.period,
            adx_threshold=args.adx_threshold, return_eff=args.return_eff,
            range_eff=args.range_eff, efficiency=args.efficiency,
            composite_adx_threshold=args.composite_adx_threshold, adx_period_cap=cap,
            exchange=args.exchange)
    explicit = _classifier_inputs_explicit(
        args.classifier, args.period, args.adx_threshold, args.return_eff, args.range_eff,
        args.efficiency, args.composite_adx_threshold, cap) or args.exchange is not None
    payload = {"symbol": args.symbol, "timeframe": args.timeframe, "target": args.target,
               "source": "model" if model else "hand_rule", "windows": report}
    if explicit:
        payload["inputs"] = _cli_inputs(args)
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
