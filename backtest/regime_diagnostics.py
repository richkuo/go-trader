"""7-state regime quality diagnostics (#1065 PR1). Pure scorers; CLI at bottom."""
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


def forward_returns(close: np.ndarray, horizon: int) -> np.ndarray:
    close = np.asarray(close, dtype=float)
    fwd = np.full(len(close), np.nan)
    if horizon < len(close):
        fwd[:-horizon] = close[horizon:] / close[:-horizon] - 1.0
    return fwd


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
    if n < 2:
        return {"transition_rate": 0.0, "flips": 0, "mean_dwell": {}}
    changes = labels[1:] != labels[:-1]
    flips = int(changes.sum())
    runs: dict[str, list[int]] = {}
    cur, length = labels[0], 1
    for x in labels[1:]:
        if x == cur:
            length += 1
        else:
            runs.setdefault(str(cur), []).append(length)
            cur, length = x, 1
    runs.setdefault(str(cur), []).append(length)
    return {
        "transition_rate": float(flips / (n - 1)),
        "flips": flips,
        "mean_dwell": {s: float(np.mean(v)) for s, v in sorted(runs.items())},
    }


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
    """Per-state forward-return significance with Benjamini-Hochberg FDR correction.

    Returns {state: {"gap": float, "p_value": float, "fdr_reject": bool}}.
    States with no in-group or no out-group bars are skipped.
    """
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


def score_labels(close, labels, features, horizons=(1, 4, 12), block_mult=3, seed=0) -> dict:
    labels = np.asarray(labels, dtype=object)
    features = np.asarray(features, dtype=float)
    # Identical NaN-mask on both label streams: warmup + low-ATR bars (NaN feature rows)
    # are excluded from coverage/stability/separation so neither the hand-rule's
    # ranging_quiet nor the model's default_label for those bars biases the comparison.
    valid = ~np.isnan(features).any(axis=1)
    vlabels = labels[valid]
    st = stability(vlabels)
    mean_dwell = float(np.mean(list(st["mean_dwell"].values()))) if st["mean_dwell"] else 1.0
    out = {"coverage": coverage(vlabels), "stability": st, "horizons": {}}
    for h in horizons:
        # Forward returns computed on the FULL close (index-aligned); then subset by valid
        # so each retained bar keeps its true h-ahead return.
        fwd_full = forward_returns(close, h)
        fwd = fwd_full[valid]
        block_len = max(int(block_mult * mean_dwell), h)
        out["horizons"][f"h{h}"] = {
            "separation": separation(vlabels, fwd),
            "significance": block_shuffle_pvalue(vlabels, fwd, block_len, seed=seed),
            "yardstick": kmeans_yardstick(features, fwd_full, seed=seed),
            "per_state_fdr": per_state_significance(vlabels, fwd, block_len, seed=seed),
        }
    # flat aliases for the pre-registered primary (h=4)
    if "h4" in out["horizons"]:
        out["h4"] = out["horizons"]["h4"]
    return out


def run_window(symbol, timeframe, window, *, model=None, horizons=(1, 4, 12), seed=0) -> dict:
    from regime import compute_regime_composite, composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    period = int(model["period"]) if model and "period" in model else 48
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats_df = composite_feature_matrix(df, period, th)
    features = feats_df.to_numpy()
    if model is None:
        labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
    else:
        from regime_hmm import forward_filter_labels
        labels, _conf = forward_filter_labels(features, model)
    return score_labels(df["close"].to_numpy(), labels, features, horizons=horizons, seed=seed)


def build_parser() -> "argparse.ArgumentParser":
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(description="7-state regime quality diagnostics (#1065)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--windows", default="is,oos", help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--model-json", default=None, help="score a fitted model instead of the hand-rule")
    p.add_argument("--horizons", default="1,4,12")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write report JSON to this path")
    return p


def main(argv=None) -> int:
    import argparse, json
    args = build_parser().parse_args(argv)
    from eval_windows import WINDOWS
    model = None
    if args.model_json:
        with open(args.model_json) as fh:
            loaded = json.load(fh)
        model = loaded.get("model", loaded) if isinstance(loaded, dict) else loaded
    horizons = tuple(int(x) for x in args.horizons.split(","))
    report = {}
    for w in args.windows.split(","):
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
        report[w] = run_window(args.symbol, args.timeframe, w, model=model,
                               horizons=horizons, seed=args.seed)
    payload = {"symbol": args.symbol, "timeframe": args.timeframe,
               "source": "model" if model else "hand_rule", "windows": report}
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
