"""Shared candidate plumbing for the #1198 squeeze_momentum regime-gate drivers.

Direct port of the #1165 breakout drivers (`backtest/candidates/breakout_1165/
driver_common.py`), re-run against squeeze_momentum (#983, same "DD is regime
exposure, not exit quality" verdict, -58.5% worst DD). The #1165 drivers were
built strategy-parameterized (`--strategy/--registry/--direction`) precisely
for this re-run; the only strategy-specific piece is the M4 "off"/"selective"
param sets below (the breakout-specific part).

Pure helpers only (no data access) so they are unit-testable
(backtest/tests/test_squeeze_momentum_1198_drivers.py) and shared by every
driver in this directory. The load-bearing one is ``candidate_leg_kwargs``:
every candidate carries regime state (``allowed_regimes`` /
``regime_windows_spec`` / ``profile_allocation``), and a driver that forgets to
thread those into ``run_leg`` silently scores the UNGATED entry — a
wrong-but-plausible number, not an error. All continuous-window drivers
(audit_headline, fee_drag) and the IS sweep build their run_leg kwargs here.

Registry note: unlike breakout (futures-only, `platforms=("futures",)`),
squeeze_momentum is registered for BOTH spot and futures with no variant
override, so its signal is identical in either registry. The drivers pin the
SPOT registry to match #983 — the baseline this study reproduces (-58.5% worst
DD) and the M5 fee audit ("gross -0.29% -> net -3.69% on spot") were both run
on spot. The issue's "futures registration" phrasing is inherited from the
breakout template, where futures was the only option; here spot keeps the
comparison against the #983 baseline valid.
"""

import statistics

# Sweep grids are versioned HERE so every artifact regenerates from the same
# candidate specs. The entry stays frozen (registry defaults); only WHEN it
# may fire varies. Strategy-specific pieces (the M4 "off"/"selective" param
# sets) are module constants so the profile grid picks them up automatically.

# M4 param sets (#998 two-profile model). "on" = frozen registry defaults
# (empty dict merges under runtime params in reg.apply_strategy). squeeze_
# momentum fires when the BB-inside-KC squeeze RELEASES (squeeze_on turns off)
# with momentum confirmation, so the analog of breakout's unreachable
# expansion multiple is a Keltner multiple pinned far above any realistic
# band width: with kc_mult=100 the Keltner channel always contains the
# Bollinger band, so squeeze_on is True on every bar and never transitions
# off — squeeze_fired is False everywhere, the profile emits no entries at all
# (the regime response is carried by the position, a flat-only switch, not the
# entry list; the frozen exit still runs). "selective" TIGHTENS the coil
# instead of zeroing it: a narrower Keltner channel (kc_mult 1.5 -> 1.3)
# demands a genuinely tighter squeeze before a release counts, and a longer
# momentum window (mom_lookback 12 -> 16) demands more sustained momentum —
# breakout's "longer channel + stricter expansion", squeeze's two core
# stringency knobs. Standalone on the IS window this keeps ~half the baseline
# entries (41 of 83 across the six datasets) — a genuine middle ground between
# baseline and off, not a near-off (kc_mult 1.0 collapses to 7).
PARAM_SET_ON: dict = {}
PARAM_SET_OFF: dict = {"kc_mult": 100.0}
PARAM_SET_SELECTIVE: dict = {"kc_mult": 1.3, "mom_lookback": 16}

ADX_SPEC = {"medium": {"classifier": "adx", "period": 14, "adx_threshold": 20.0}}
COMPOSITE_SPEC = {"medium": {"classifier": "composite", "period": 14}}

# Composite 9-state vocabulary (#1058/#1124). The bare `ranging_directional`
# in an allowed set covers its `_up`/`_down` subs (bare-covers-subs), so the
# not_down sets list the bare label only.
COMP_UP_FAMILY = ["trending_up_clean", "trending_up_choppy"]
COMP_NOT_DOWN = COMP_UP_FAMILY + ["ranging_quiet", "ranging_volatile",
                                  "ranging_directional"]
COMP_NOT_DOWN_CALM = COMP_UP_FAMILY + ["ranging_quiet", "ranging_directional"]


def adx_spec(threshold: float) -> dict:
    """ADX windows spec at a non-default gate threshold (plateau sweeps)."""
    return {"medium": {"classifier": "adx", "period": 14,
                       "adx_threshold": float(threshold)}}


def gate_candidate(label: str, allowed: list, spec: dict) -> dict:
    """Arm A: entry gate on the frozen entry + frozen default close stack."""
    return {"label": label, "allowed_regimes": list(allowed),
            "regime_windows_spec": {k: dict(v) for k, v in spec.items()}}


def profile_candidate(label: str, profiles: dict, window_spec: dict,
                      off_set: dict = PARAM_SET_OFF) -> dict:
    """Arm B: #998 two-profile allocation (flat-only switch, confirm_bars 2).

    ``profiles`` maps every regime label of the window's classifier to
    "on"/"off" explicitly — the switcher holds the active profile on unknown
    labels (fail-open), so an unmapped label would silently mean "no change",
    not "off".
    """
    return {"label": label, "profile_allocation": {
        "window_spec": dict(window_spec),
        "profiles": dict(profiles),
        "param_sets": {"on": dict(PARAM_SET_ON), "off": dict(off_set)},
        "confirm_bars": 2,
        "initial_profile": "on",
    }}


def build_gate_grid() -> list:
    """Arm A screen rows: ADX label subsets + composite label subsets."""
    rows = [
        {"label": "baseline"},
        # ADX (3-label vocabulary), gate threshold 20 = live default.
        gate_candidate("adx_up", ["trending_up"], ADX_SPEC),
        gate_candidate("adx_not_down", ["trending_up", "ranging"], ADX_SPEC),
        gate_candidate("adx_trend_only", ["trending_up", "trending_down"],
                       ADX_SPEC),
        # Composite 9-state (#1058/#1124), period 14 medium window.
        gate_candidate("comp_up_family", COMP_UP_FAMILY, COMPOSITE_SPEC),
        gate_candidate("comp_up_clean", ["trending_up_clean"], COMPOSITE_SPEC),
        gate_candidate("comp_not_down", COMP_NOT_DOWN, COMPOSITE_SPEC),
        gate_candidate("comp_not_down_calm", COMP_NOT_DOWN_CALM,
                       COMPOSITE_SPEC),
        gate_candidate("comp_up_plus_dir_up",
                       COMP_UP_FAMILY + ["ranging_directional_up"],
                       COMPOSITE_SPEC),
    ]
    return rows


def build_gate_threshold_plateau(allowed: list,
                                 thresholds=(15.0, 25.0, 30.0)) -> list:
    """ADX gate-threshold plateau around the default 20 (M1 step 6: the pick
    must sit on a shelf, not a spike)."""
    return [gate_candidate(f"adx_gate_t{t:g}", allowed, adx_spec(t))
            for t in thresholds]


def build_composite_period_plateau(allowed: list,
                                   periods=(10, 21, 28)) -> list:
    """Composite classifier-period plateau around the default 14 (M1 step 6
    for the composite gate arm — the winning label set must hold across
    neighboring window periods, not spike at one)."""
    return [gate_candidate(
        f"comp_gate_p{p}", allowed,
        {"medium": {"classifier": "composite", "period": int(p)}})
        for p in periods]


def build_profile_grid() -> list:
    """Arm B screen rows: M4 profile allocation on ADX and composite switch
    windows."""
    adx_ws = dict(ADX_SPEC["medium"])
    comp_ws = dict(COMPOSITE_SPEC["medium"])
    comp_profiles_bear_off = {
        "trending_up_clean": "on", "trending_up_choppy": "on",
        "ranging_quiet": "on", "ranging_volatile": "on",
        "ranging_directional": "on", "ranging_directional_up": "on",
        "ranging_directional_down": "on",
        "trending_down_clean": "off", "trending_down_choppy": "off",
    }
    return [
        profile_candidate("m4_bear_off",
                          {"trending_up": "on", "ranging": "on",
                           "trending_down": "off"}, adx_ws),
        profile_candidate("m4_trend_only",
                          {"trending_up": "on", "ranging": "off",
                           "trending_down": "off"}, adx_ws),
        profile_candidate("m4_bear_selective",
                          {"trending_up": "on", "ranging": "on",
                           "trending_down": "off"}, adx_ws,
                          off_set=PARAM_SET_SELECTIVE),
        profile_candidate("m4_comp_bear_off", comp_profiles_bear_off, comp_ws),
    ]


def candidate_leg_kwargs(candidate: dict) -> dict:
    """run_leg kwargs for one candidate — the single place regime state is
    threaded, so a gated candidate can never silently run ungated on the
    continuous audit window (the #983 close-stack drivers didn't need this;
    every #1198 one does)."""
    return dict(
        close_strategies=candidate.get("close_strategies"),
        direction=candidate.get("direction") or "long",
        invert_signal=bool(candidate.get("invert_signal")),
        stop_loss_atr_mult=candidate.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=candidate.get("trailing_stop_atr_mult"),
        profile_allocation=candidate.get("profile_allocation"),
        allowed_regimes=candidate.get("allowed_regimes"),
        regime_windows_spec=candidate.get("regime_windows_spec"),
        regime_directional_policy=candidate.get("regime_directional_policy"),
    )


def summarize_fee_drag(gross_legs, net_legs):
    """Collapse paired (gross, net) leg dicts into the fee-drag summary.

    Pure: takes two equal-length lists of leg dicts (each needs ``return_pct``,
    ``trades``, ``span_days``; ``None`` legs are dropped pairwise) and returns
    mean gross/net return %, drag in pp, summed trades, and trades/yr
    annualized over the summed calendar span. Returns None when no paired legs
    survive. Inlined here (rather than reaching into a twin directory as #1165
    reached into breakout_984) so this research directory is self-contained and
    the aggregation is unit-tested alongside its own drivers.
    """
    pairs = [(g, n) for g, n in zip(gross_legs, net_legs)
             if g is not None and n is not None]
    if not pairs:
        return None
    gross = [g["return_pct"] for g, _ in pairs]
    net = [n["return_pct"] for _, n in pairs]
    trades = sum(n["trades"] for _, n in pairs)
    span_days = sum(float(n.get("span_days") or 0.0) for _, n in pairs)
    mean_gross = statistics.mean(gross)
    mean_net = statistics.mean(net)
    return {
        "legs": len(pairs),
        "mean_gross_return_pct": round(mean_gross, 2),
        "mean_net_return_pct": round(mean_net, 2),
        "drag_pp": round(mean_gross - mean_net, 2),
        "trades": trades,
        "trades_per_year": (round(trades / (span_days / 365.25), 1)
                            if span_days > 0 else None),
    }
