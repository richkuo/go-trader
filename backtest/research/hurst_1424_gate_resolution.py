#!/usr/bin/env python3
"""#1424: Hurst entry-gate RESOLUTION study — report only.

The #1422 power study (``hurst_1422_gate_power.py``) returned ``INCONCLUSIVE``
because of POWER and not because of an absent signal. Its primary cohort
resolved 0.96 pp per trade under the cluster null while the largest anti-signal
separation on those same rows was 0.37 pp. A null measured under a limit ABOVE
the separation bounds the edge from above and says nothing at all about a
smaller one, so the question "does Hurst carry a usable edge" stayed open and
that design could not close it.

This successor exists to make its OWN null readable. It lowers the detection
limit by three independent, pre-registered routes that combine:

1. ONE pre-registered primary hypothesis instead of four. The rank-1
   Benjamini-Hochberg bar moves from ``alpha/4`` to ``alpha``. The pick is
   derived MECHANICALLY as the smallest raw p in the COMMITTED #1410 JSON —
   evidence that never read one bar of the tape #1422 scored for the first
   time, which is what lets the primary cohort here include #1422's primary
   cells as well as the new pre-2020 ones. The cost is stated plainly: this
   study tests ONE gate design and cannot rank alternatives.
2. More INDEPENDENT calendar clusters. Effective N is set by how many
   independent calendar spans a pool holds, not by trade count, so this adds
   pre-2020 history from two venues whose tape the ``binanceus`` cache does not
   reach: Bitstamp BTC/USD back to 2013 and Coinbase Exchange BTC/ETH/LTC back
   to 2016. Every (base asset, calendar window) cell has exactly ONE owning
   venue, asserted, so no calendar span is ever counted twice.
3. A lower-variance primary target. Per-trade net return is the noisiest target
   available. The pre-registered inference statistic becomes SIGNED FIXED-HORIZON
   EFFICIENCY, bounded in ``[-1, 1]``, which removes the fat tail. It is an
   OUTCOME, exactly as PnL is, so no look-ahead invariant moves. A win on it is
   evidence about the MECHANISM and is not by itself a licence to ship a gate —
   the acceptance rule still requires the net-return economics to clear.

VALIDITY GATE, pre-registered and mechanical. This study is valid only when the
measured cluster-null detection limit on its primary cohort, in efficiency
units, falls BELOW that same cohort's pool-matched observed separation on that
same statistic. When it does not, the report says so plainly and the verdict
stays a power statement — never an assertion about the market.

REPORT-PATH CONTRACT. This study OWNS ``backtest/research/hurst_gate_calibration.md``
— the live-evidence path cited by ``scheduler/hurst_gate.go``,
``docs/ARCHITECTURE.md`` and #1412's Stage 0 gate. #1422's own render now lives
at ``hurst_1422_gate_power.md`` and #1410's at ``hurst_1410_gate_calibration.md``;
neither study may write this path again.

REPORT-ONLY. Zero scheduler, config, gating, sizing, or live-path changes. The
gate and size arms are OFFLINE SIMULATIONS. #1411's ``hurst_gate`` ships
default-off with no recommended thresholds and is untouched by this study;
whether that stays true is what the Recommendation section decides.

Method
------
Inherited verbatim from #1422 unless named here: the #1409 ``hurst_exponent``
SSoT, the rolling-H look-ahead convention, the NaN policy, the shared circular
calendar-rotation null, effective N, the coverage density floor, the dedup rule,
the harness-identity mirror against ``eval_windows.run_leg``, and the joint
ADX x Hurst section.

What is new: the venue-qualified dataset identity and its window-ownership
matrix, the signed-efficiency primary target, the single-hypothesis primary
family, the 4-window primary economics protocol, and the validity gate.

Usage
-----
  uv run --no-sync python backtest/research/hurst_1424_gate_resolution.py \
      --jobs 8 --write-report --out-dir /tmp/hurst1424
  uv run --no-sync python backtest/research/hurst_1424_gate_resolution.py --render-only
  uv run --no-sync python backtest/research/hurst_1424_gate_resolution.py --fetch-only
"""

import argparse
import json
import math
import os
import sys
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

from eval_windows import (  # noqa: E402  (sys.path bootstrap must run first)
    DATASETS as AUDIT_DATASETS,
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    HELD_OUT_WINDOWS as AUDIT_HELD_OUT_WINDOWS,
    PLATFORM,
    PROTOCOL_WINDOWS as AUDIT_PROTOCOL_WINDOWS,
    WINDOWS as AUDIT_WINDOWS,
    dataset_key,
    leg_from_results,
    run_leg,
)
from regime_stats import benjamini_hochberg  # noqa: E402

# The two predecessor studies are the SSoT for every helper this study did not
# have to change. Imported by unambiguous module name off research/ on sys.path
# — the pattern the #1410 and #1422 test modules use, and the one that stays
# safe under the #1304 `pytest -n auto` parallel run.
import hurst_1410_gate_calibration as study1410  # noqa: E402
import hurst_1422_gate_power as study1422  # noqa: E402

# --- #1410 (estimator, buckets, gate/size mechanics) -----------------------
bucket_label = study1410.bucket_label
cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
chop_loss = study1410.chop_loss
compound_equity = study1410.compound_equity
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
hysteresis_mask = study1410.hysteresis_mask
permutation_pvalue_group_diff = study1410.permutation_pvalue_group_diff
permutation_pvalue_weighted = study1410.permutation_pvalue_weighted
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
size_multiplier = study1410.size_multiplier
slice_window = study1410.slice_window
warmup_audit = study1410.warmup_audit
warmup_lead_bars = study1410.warmup_lead_bars
win_rate = study1410.win_rate

BUCKETS = study1410.BUCKETS
BUCKET_NAN = study1410.BUCKET_NAN
EXEMPLAR_CLOSE_OVERRIDES = study1410.EXEMPLAR_CLOSE_OVERRIDES
FAMILIES = study1410.FAMILIES
FAMILY_EXEMPLARS = study1410.FAMILY_EXEMPLARS
FAMILY_SENSE = study1410.FAMILY_SENSE
GATE_INITIAL_ARMED = study1410.GATE_INITIAL_ARMED
GATE_PAIRS = study1410.GATE_PAIRS
HURST_WINDOWS = study1410.HURST_WINDOWS
SENSE_HIGH = study1410.SENSE_HIGH
SENSE_LOW = study1410.SENSE_LOW
SIZING_CLAMP_HI = study1410.SIZING_CLAMP_HI
SIZING_CLAMP_LO = study1410.SIZING_CLAMP_LO
SIZING_GAINS = study1410.SIZING_GAINS
SIZING_NAN_MULTIPLIER = study1410.SIZING_NAN_MULTIPLIER
CONFIG_ID_SEP = study1410.CONFIG_ID_SEP
gate_config_id = study1410.gate_config_id
size_config_id = study1410.size_config_id
_MIRRORED_LEG_KEYS = study1410._MIRRORED_LEG_KEYS

# --- #1422 (cluster null, effective N, coverage, MDE machinery) ------------
adx_entry_stamp = study1422.adx_entry_stamp
anti_signal_side = study1422.anti_signal_side
cluster_permutation_pvalue_group_diff = study1422.cluster_permutation_pvalue_group_diff
cluster_permutation_pvalue_weighted = study1422.cluster_permutation_pvalue_weighted
cluster_rotation_offsets = study1422.cluster_rotation_offsets
dedup_entries = study1422.dedup_entries
effective_n = study1422.effective_n
expected_bars = study1422.expected_bars
joint_adx_bucket = study1422.joint_adx_bucket
joint_h_bucket = study1422.joint_h_bucket
timeframe_minutes = study1422.timeframe_minutes
trade_samples_with_span = study1422.trade_samples_with_span
usable_cluster_rows = study1422.usable_cluster_rows
_rank1_threshold = study1422._rank1_threshold
_separation = study1422._separation

ADX_PERIOD = study1422.ADX_PERIOD
ADX_SPLIT = study1422.ADX_SPLIT
COHORT_EXPLORATORY = study1422.COHORT_EXPLORATORY
COHORT_PRIMARY = study1422.COHORT_PRIMARY
D_1410 = study1422.D_1410
JOINT_ADX_BUCKETS = study1422.JOINT_ADX_BUCKETS
JOINT_H_BUCKETS = study1422.JOINT_H_BUCKETS
MIN_CLUSTER_SPAN_DAYS = study1422.MIN_CLUSTER_SPAN_DAYS
MIN_OFFSET_DAYS = study1422.MIN_OFFSET_DAYS
MIN_WINDOW_BAR_FRACTION = study1422.MIN_WINDOW_BAR_FRACTION
NO_JOINT_SEPARATION = study1422.NO_JOINT_SEPARATION

# ---------------------------------------------------------------------------
# Pre-registered design constants. Fixed before the sweep ran; serialized
# verbatim into the JSON and the report so a reader can tell the Recommendation
# was not tuned after seeing the numbers.
# ---------------------------------------------------------------------------
SCHEMA_VERSION = 1
ISSUE = 1424
SEED = ISSUE  # 1424 — fixed so a re-run reproduces every p-value

# --- Route 1: ONE pre-registered primary hypothesis ------------------------
# Derived MECHANICALLY as argmin(p_raw) over ALL configs in the committed #1410
# JSON, and pinned here so a regenerated #1410 JSON can never silently swap the
# pre-registration. `resolve_primary_config_id` re-derives it at run time and
# `main` asserts the two agree before a single leg is scored.
#
# Selecting on #1410's outcomes is legitimate for a cohort disjoint from the
# data those outcomes came from. That is why the primary cohort below may
# include #1422's primary cells as well as the new pre-2020 ones: the pick
# never read either. What it DID read is disclosed as an interim look.
PRIMARY_CONFIG_ID = "momentum/gate/W512/arm0.52/dis0.48"
PRIMARY_CONFIG_IDS = (PRIMARY_CONFIG_ID,)
# The Benjamini-Hochberg denominator for the confirmatory family. One.
PRIMARY_FAMILY_SIZE = len(PRIMARY_CONFIG_IDS)
# The family that hypothesis belongs to. `mean_reversion` is EXPLORATORY-ONLY
# in this study and can never produce a recommendation.
PRIMARY_FAMILY = PRIMARY_CONFIG_ID.split(CONFIG_ID_SEP)[0]

INTERIM_LOOK_DISCLOSURE = (
    "INTERIM LOOK, disclosed before the run. The #1422 study already published "
    "a cluster p of 0.0485 for this exact configuration on the subset of the "
    "primary cohort it scored (its own pre-2023 windows and non-audit "
    "symbols). That number was seen before this study's primary cohort was "
    "fixed. The bias runs in ONE direction and it is the optimistic one: an "
    "interim look that came out encouraging makes a confirmatory re-test on a "
    "SUPERSET of those same rows more likely to look good than a clean "
    "pre-registration would. The pre-registration this study can honestly "
    "claim is over the CONFIGURATION, which came from #1410's evidence alone, "
    "and over the added pre-2020 tape, which nothing had scored. It cannot "
    "claim the inherited rows are naive. A reader should discount a marginal "
    "positive on the primary hypothesis accordingly, and should read a NULL "
    "as unaffected — the bias cannot manufacture one.")

KEY_RISK_PREDICTION = (
    "The visible separations are consistent with whole datasets moving "
    "together, not with H sorting trades within them; a higher-power design "
    "may therefore confirm a null; if it does, that outcome closes the "
    "question as 'no usable Hurst edge at or above the new limit'.")

# --- Route 2: more independent calendar clusters ---------------------------
BITSTAMP = "bitstamp"
COINBASE = "coinbaseexchange"

# Recorded feasibility probes. Run read-only against each candidate source
# BEFORE the design was frozen, so the report can state which routes were
# options and which were not, instead of quietly omitting the ones that failed.
FEASIBILITY_PROBES = (
    {
        "source": "bitstamp (ccxt)",
        "verdict": "USED",
        "detail": "BTC/USD serves 1h and 4h candles from 2012-01-01, up to "
                  "1000 per page. ETH/USD, LTC/USD and XRP/USD return nothing "
                  "before 2017, so only BTC/USD is scored from this venue.",
    },
    {
        "source": "coinbaseexchange (ccxt)",
        "verdict": "USED",
        "detail": "measured first 1h candle: BTC/USD 2015-08, ETH/USD 2016-06, "
                  "LTC/USD 2016-09. A page is CAPPED at 300 candles, and a "
                  "request before a pair's listing returns an EMPTY page that "
                  "`fetch_full_history` reads as end-of-history — so each of "
                  "these datasets carries its own measured history floor rather "
                  "than one venue-wide date. ETH and LTC therefore have no lead "
                  "before the `2016` window and the coverage audit drops those "
                  "two cells loudly. The venue publishes no 4h granularity "
                  "(1m/5m/15m/1h/6h/1d only), so these datasets are 1h-only.",
    },
    {
        "source": "binance (global, ccxt)",
        "verdict": "INFEASIBLE",
        "detail": "HTTP 451 from this machine's region on /api/v3/exchangeInfo, "
                  "so no candle request can be issued at all.",
    },
    {
        "source": "kraken (ccxt)",
        "verdict": "INFEASIBLE",
        "detail": "ignores `since` for deep history — a 2012 request returns "
                  "the trailing ~720 candles — so it carries no pre-2020 tape.",
    },
    {
        "source": "TopStep futures adapter (platforms/topstep)",
        "verdict": "INFEASIBLE",
        "detail": "`get_ohlcv` takes only a `limit`, never a `since`, so it is "
                  "a trailing-window surface and cannot be backfilled to a "
                  "chosen year. Its paper fallback is yfinance, which caps 1h "
                  "history at roughly 730 days; live depth is unverifiable "
                  "here without credentials, and either way the missing "
                  "`since` decides it. The instrument-family route Route 2 "
                  "names is therefore NOT an option in this repository today.",
    },
    {
        "source": "IBKR adapter (platforms/ibkr)",
        "verdict": "INFEASIBLE",
        "detail": "exposes no OHLCV surface at all — spot price, volatility, "
                  "expiry, strike and premium/greeks only.",
    },
)

# Per-page candle caps, measured above. `fetch_full_history` advances by the
# LAST RETURNED candle, so a short page is safe; passing the real cap only
# stops an oversized request being rejected outright.
FETCH_PAGE_LIMIT = {PLATFORM: 500, BITSTAMP: 1000, COINBASE: 300}

# Per-venue history floors. Each is early enough to give the deepest Hurst
# window its required lead before that venue's earliest owned window.
HISTORY_SINCE = {
    PLATFORM: study1422.HISTORY_SINCE,   # "2020-01-01", #1422's floor verbatim
    BITSTAMP: "2012-06-01",
    COINBASE: "2015-08-01",
}

# Per-DATASET overrides, set to each pair's MEASURED first candle. This is not
# cosmetic: `fetch_full_history` stops at the first empty page, and a venue
# answers a request before a pair's listing with an empty page. A single
# venue-wide floor earlier than the latest-listed pair therefore backfills
# NOTHING for that pair, silently, and the study would score whatever the cache
# already held. Every floor here was measured, and the coverage audit still has
# the last word on which cells are scoreable.
DATASET_HISTORY_SINCE = {
    (COINBASE, "BTC/USD", "1h"): "2015-08-01",
    (COINBASE, "ETH/USD", "1h"): "2016-06-01",
    (COINBASE, "LTC/USD", "1h"): "2016-09-01",
}


def history_since_for(dataset: tuple) -> str:
    """The backfill floor for one dataset: its own measured listing date, else
    its venue's default."""
    dataset = tuple(dataset)
    override = DATASET_HISTORY_SINCE.get(dataset)
    if override:
        return override
    return HISTORY_SINCE.get(dataset[0], study1422.HISTORY_SINCE)

# The eight PRE-2020 windows this study adds. All are strictly before
# 2020-07-01, which is what makes "new venue implies primary cohort" sound.
PRE2020_WINDOWS = {
    "2013":   ("2013-01-01", "2014-01-01"),
    "2014":   ("2014-01-01", "2015-01-01"),
    "2015":   ("2015-01-01", "2016-01-01"),
    "2016":   ("2016-01-01", "2017-01-01"),
    "2017":   ("2017-01-01", "2018-01-01"),
    "2018":   ("2018-01-01", "2019-01-01"),
    "2019":   ("2019-01-01", "2020-01-01"),
    "2020H1": ("2020-01-01", "2020-07-01"),
}
# #1422's eight windows are reused VERBATIM from that module — never redefined
# here, so a drift there fails loud below instead of silently rescoring the
# inherited cells on different spans.
WINDOWS = dict(study1422.WINDOWS)
for _k, _v in PRE2020_WINDOWS.items():
    if _k in WINDOWS:
        raise AssertionError(f"pre-2020 window {_k!r} collides with a #1422 window")
    WINDOWS[_k] = _v

WINDOW_ORDER = tuple(sorted(WINDOWS, key=lambda w: (WINDOWS[w][0], w)))

# The base symbols #1410 scored, inherited so the exploratory cohort keeps its
# meaning.
AUDIT_SYMBOLS = study1422.AUDIT_SYMBOLS

# Datasets are (exchange_id, symbol, timeframe) TRIPLES here. #1422's 21
# binanceus datasets are inherited verbatim; the new-venue ones are the
# feasibility-verified pre-2020 sources.
BINANCEUS_DATASETS = [(PLATFORM, s, t) for (s, t) in study1422.DATASETS]
NEW_VENUE_DATASETS = [
    (BITSTAMP, "BTC/USD", "1h"),
    (BITSTAMP, "BTC/USD", "4h"),
    (COINBASE, "BTC/USD", "1h"),
    (COINBASE, "ETH/USD", "1h"),
    (COINBASE, "LTC/USD", "1h"),
]
DATASETS = BINANCEUS_DATASETS + NEW_VENUE_DATASETS

BITSTAMP_WINDOWS = ("2013", "2014", "2015")
COINBASE_WINDOWS = ("2016", "2017", "2018", "2019", "2020H1")
BINANCEUS_WINDOWS = tuple(study1422.WINDOW_ORDER)

# WINDOW OWNERSHIP MATRIX. Exactly one venue owns each (base asset, window)
# cell, so no calendar span is ever scored twice through two venues' tape.
# Asserted at import by `_assert_window_ownership`; a future edit that breaks
# it fails loud rather than silently double-counting a year.
DATASET_WINDOWS = {}
for _ds in BINANCEUS_DATASETS:
    DATASET_WINDOWS[_ds] = BINANCEUS_WINDOWS
for _ds in NEW_VENUE_DATASETS:
    DATASET_WINDOWS[_ds] = (BITSTAMP_WINDOWS if _ds[0] == BITSTAMP
                            else COINBASE_WINDOWS)

# --- Route 3: the lower-variance primary target ----------------------------
# Signed fixed-horizon Kaufman efficiency. For a trade FILLED at bar i in
# direction d, over K bars: d * (close[i+K] - close[i]) divided by the summed
# absolute bar-to-bar path over the same span. Bounded in [-1, 1] by
# construction (the numerator's magnitude can never exceed the path), which is
# exactly what removes the fat tail per-trade net return carries.
#
# It is an OUTCOME, like PnL. Nothing about H's or ADX's look-ahead convention
# changes: both still read strictly closed bars before the fill.
VERDICT_CONFIG = "config"
VERDICT_RESOLVED_NULL = "resolved_null"
VERDICT_INCONCLUSIVE = "inconclusive"
VERDICT_LABELS = {
    VERDICT_CONFIG: "CONFIGURATION RECOMMENDED",
    VERDICT_RESOLVED_NULL: "NO USABLE HURST EDGE AT OR ABOVE THE MEASURED LIMIT",
    VERDICT_INCONCLUSIVE: "INCONCLUSIVE",
}

PRIMARY_TARGET = "signed_fixed_horizon_efficiency"
CONTINUITY_TARGET = "pnl_pct_net"
HORIZON_HOURS = 96            # four days, the same order as a held position
EFFICIENCY_EPS = 1e-12        # a dead-flat span yields 0, never a division error

# --- Inference -------------------------------------------------------------
N_PERM = study1422.N_PERM             # 10000
N_PERM_MDE = study1422.N_PERM_MDE     # 2000
ALPHA = study1422.ALPHA               # 0.05
JOINT_ALPHA = study1422.JOINT_ALPHA   # ALPHA / len(FAMILIES)

# Minimum-detectable-effect search grid IN EFFICIENCY UNITS. Coarse pass then
# one refinement, exactly #1422's shape; only the units and the step sizes
# differ, because the target is bounded in [-1, 1] rather than measured in
# percentage points. The coarse step is 0.02 rather than the 0.002 the plan
# sketched: `min_detectable_effect` scans upward and stops at the first grid
# point that clears the bar, so a 0.002 step would make the run cost scale with
# the answer's magnitude, and the 0.001 refinement recovers the same final
# RESOLUTION at a bounded cost of at most 46 permutation batches.
MDE_EFF_GRID_STEP = 0.02
MDE_EFF_GRID_MAX = 0.5
MDE_EFF_REFINE_STEP = 0.001
# The net-return grid is #1422's verbatim, so the two studies' continuity
# limits stay directly comparable.
MDE_PP_GRID_STEP = study1422.MDE_GRID_STEP
MDE_PP_GRID_MAX = study1422.MDE_GRID_MAX
MDE_PP_REFINE_STEP = study1422.MDE_REFINE_STEP

# --- Decision floors and economics ----------------------------------------
MIN_SUPPRESSED_EFFECTIVE = study1422.MIN_SUPPRESSED_EFFECTIVE   # 20.0
MIN_KEPT_EFFECTIVE = study1422.MIN_KEPT_EFFECTIVE               # 30.0
RETURN_TOLERANCE_PP = study1422.RETURN_TOLERANCE_PP             # 1.0
RETURN_TOLERANCE_FRAC = study1422.RETURN_TOLERANCE_FRAC         # 0.1
HELD_OUT_MIN_FRACTION = study1422.HELD_OUT_MIN_FRACTION         # 2/3
HELD_OUT_MIN_WINDOWS = study1422.HELD_OUT_MIN_WINDOWS           # 3

# Primary economics protocol: two bull years and two bear years, so a winner
# cannot be a one-regime artifact. #1422 required EVERY protocol window to
# hold; with four windows spanning two market characters that would be a
# harsher bar than the evidence warrants, so the pre-registered rule is 3 of
# the 4 THAT CARRY LEGS, with at least 3 carrying legs for the check to be
# testable at all.
PRIMARY_PROTOCOL_WINDOWS = ("2017", "2018", "2021", "2022")
PRIMARY_PROTOCOL_MIN_WINDOWS = 3
PRIMARY_HELD_OUT_WINDOWS = tuple(
    w for w in WINDOW_ORDER if w not in PRIMARY_PROTOCOL_WINDOWS)
# The exploratory arm keeps #1410's split byte-identical.
EXPLORATORY_PROTOCOL_WINDOWS = tuple(AUDIT_PROTOCOL_WINDOWS)
EXPLORATORY_PROTOCOL_MIN_WINDOWS = len(EXPLORATORY_PROTOCOL_WINDOWS)
EXPLORATORY_HELD_OUT_WINDOWS = tuple(AUDIT_HELD_OUT_WINDOWS)

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1424_gate_resolution.json")
# The live-evidence contract path. #1412 Stage 0, scheduler/hurst_gate.go and
# docs/ARCHITECTURE.md all read this file. This study owns it.
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_gate_calibration.md")
_JSON_1410 = os.path.join(_THIS_DIR, "hurst_1410_gate_calibration.json")


# ---------------------------------------------------------------------------
# Dataset identity. A venue-qualified symbol, because two venues list the same
# pair.
# ---------------------------------------------------------------------------

def qualified_symbol(exchange_id: str, symbol: str) -> str:
    """The symbol label this study pools trades under.

    ``binanceus`` keeps the PLAIN symbol, so every inherited comparison —
    ``AUDIT_SYMBOLS`` membership, the ``D_1410`` cell set, #1422's own cohort
    rule — keeps working byte-identically on the inherited datasets. Any other
    venue is suffixed, because Bitstamp's ``BTC/USD`` and Coinbase's
    ``BTC/USD`` are DIFFERENT tapes and must never merge into one rotation
    cluster, one dedup key, or one effective-N group.
    """
    return symbol if exchange_id == PLATFORM else f"{symbol}@{exchange_id}"


def base_asset(symbol: str) -> str:
    """The base asset of a (possibly venue-qualified) symbol.

    ``BTC/USDT``, ``BTC/USD`` and ``BTC/USD@bitstamp`` are all ``BTC`` — one
    asset whose tape is one tape whatever the quote currency or the venue.
    """
    return str(symbol).split("@", 1)[0].split("/", 1)[0].strip().upper()


def _assert_window_ownership(dataset_windows: dict) -> dict:
    """Exactly one venue may own each (base asset, window) cell.

    This is the assertion that lets the pool add pre-2020 venues without
    double-counting a calendar span. Bitstamp BTC 2013-2015, Coinbase BTC
    2016-2020H1 and Binance.US BTC 2020H2-onward are three disjoint stretches of
    ONE asset's history; scoring two venues over the same year would put the
    same market twice into a null whose whole job is to count independent
    calendar clusters.

    Returns ``{f"{asset}|{window}": exchange_id}`` for the report.
    """
    owner: dict = {}
    for (exchange_id, symbol, _tf), windows in sorted(dataset_windows.items()):
        asset = base_asset(symbol)
        for window in windows:
            if window not in WINDOWS:
                raise AssertionError(
                    f"{exchange_id} {symbol} owns unknown window {window!r}")
            key = f"{asset}|{window}"
            held = owner.get(key)
            if held is not None and held != exchange_id:
                raise AssertionError(
                    f"window ownership collision on {key}: both {held!r} and "
                    f"{exchange_id!r} claim it; one calendar span would be "
                    f"counted twice")
            owner[key] = exchange_id
    return owner


WINDOW_OWNER = _assert_window_ownership(DATASET_WINDOWS)

# "New venue implies primary cohort" is only sound while every new-venue window
# ends before #1422's earliest primary window begins. Assert it rather than
# trust the table above.
_PRIMARY_FLOOR = pd.Timestamp("2020-07-01")
for _w in set(BITSTAMP_WINDOWS) | set(COINBASE_WINDOWS):
    if pd.Timestamp(WINDOWS[_w][1]) > _PRIMARY_FLOOR:
        raise AssertionError(
            f"new-venue window {_w!r} runs past {_PRIMARY_FLOOR.date()}, so it "
            f"is not automatically primary; re-derive cell_cohort first")


# ---------------------------------------------------------------------------
# Pure helpers (unit-tested without data access).
# ---------------------------------------------------------------------------

def cell_cohort(exchange_id: str, symbol: str, timeframe: str,
                window_name: str) -> str:
    """Which inference cohort one (dataset, window) cell belongs to.

    PRIMARY iff the cell's tape was NOT read by the evidence that selected the
    primary hypothesis. That evidence is the committed #1410 JSON and nothing
    else, so:

      * every new-venue cell is primary — #1410 never saw a Bitstamp or
        Coinbase bar, and (asserted above) every such window is pre-2020H2;
      * a ``binanceus`` cell is primary when its window or its symbol is one
        #1410 never scored — #1422's rule verbatim, which is deliberately
        COARSER than the (dataset, window) grid: BTC 2h over a #1410 window is
        the same tape resampled and stays exploratory.

    The #1422 primary cells are therefore primary HERE too. That is the whole
    of Route 1's dividend and it is legitimate for one reason only: the
    hypothesis was picked from #1410's p-values, which never read those cells.
    What #1422 published ABOUT those cells was seen, and that interim look is
    disclosed in ``INTERIM_LOOK_DISCLOSURE`` rather than hidden.
    """
    if window_name not in WINDOWS:
        raise ValueError(f"unknown window {window_name!r}")
    if exchange_id != PLATFORM:
        return COHORT_PRIMARY
    if window_name not in AUDIT_WINDOWS:
        return COHORT_PRIMARY
    return COHORT_EXPLORATORY if symbol in AUDIT_SYMBOLS else COHORT_PRIMARY


def horizon_bars(timeframe: str, horizon_hours: int = HORIZON_HOURS) -> int:
    """Bars in the fixed efficiency horizon for one timeframe.

    The horizon is fixed in CALENDAR time, not in bars, so a 1h and a 4h
    dataset measure follow-through over the same four days and their efficiency
    values are directly poolable. A timeframe coarser than the horizon would
    make the target meaningless and raises instead of silently rounding to one
    bar.
    """
    minutes = timeframe_minutes(timeframe)
    total = int(horizon_hours) * 60
    if minutes <= 0 or minutes > total:
        raise ValueError(
            f"timeframe {timeframe!r} is not finer than the {horizon_hours}h "
            f"efficiency horizon")
    return total // minutes


def signed_efficiency(closes, pos: int, k: int, direction: int) -> Optional[float]:
    """Signed fixed-horizon Kaufman efficiency for one fill, or None.

    ``d * (close[pos+k] - close[pos]) / (sum |close[j+1] - close[j]| + eps)``
    over ``j`` in ``[pos, pos+k)``. The numerator's magnitude can never exceed
    the denominator's path, so the result is bounded in ``[-1, 1]`` — that
    bound is the entire point of the target, and it is what lets the same rows
    resolve a smaller effect than per-trade net return can.

    Returns None when the horizon runs off the end of the scored slice. Such a
    trade is EXCLUDED from the contrast and counted in the report, never
    silently truncated to a shorter horizon (which would mix two different
    statistics in one pool).
    """
    arr = np.asarray(closes, dtype=float)
    i = int(pos)
    k = int(k)
    if k < 1:
        raise ValueError(f"efficiency horizon must be >= 1 bar, got {k}")
    if i < 0 or i + k >= arr.size:
        return None
    segment = arr[i:i + k + 1]
    if not np.all(np.isfinite(segment)):
        return None
    net = float(direction) * float(segment[-1] - segment[0])
    path = float(np.sum(np.abs(np.diff(segment))))
    return float(net / (path + EFFICIENCY_EPS))


def trade_direction(side) -> int:
    """+1 for a long fill, -1 for a short one.

    Raises on anything else. A silent default would flip the sign of the
    primary target for a whole leg, which is the one error this study could
    not detect downstream — an inverted contrast reads exactly like a real
    anti-signal.
    """
    label = str(side or "").strip().lower()
    if label == "long":
        return 1
    if label == "short":
        return -1
    raise ValueError(f"unknown trade side {side!r}")


def min_detectable_effect_on_grid(trades: Sequence[dict],
                                  values: Sequence[float],
                                  suppressed: Sequence[bool],
                                  family_size: int,
                                  *,
                                  grid_step: float,
                                  grid_max: float,
                                  refine_step: float,
                                  cluster: bool = True,
                                  n_perm: int = N_PERM_MDE,
                                  seed: int = SEED,
                                  alpha: float = ALPHA) -> Optional[float]:
    """#1422's detection-limit search, parameterized on the injection grid.

    Identical machinery — a deterministic shift of every SUPPRESSED value by
    ``d``, a coarse upward scan, then one refinement pass, scored against the
    rank-1 Benjamini-Hochberg bar — with the grid supplied by the caller so the
    same estimator serves the bounded efficiency target and the percentage-point
    continuity target without either borrowing the other's units.

    Returns None when even the largest grid point cannot clear the bar. Raises
    when the permutation count cannot RESOLVE that bar: with the add-one
    convention the smallest reachable p is ``1/(n_perm+1)``, so too few draws
    would make every effect look undetectable and publish "no power" when the
    truth is "no permutations". That failure must be loud.
    """
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    bar = _rank1_threshold(family_size, alpha)
    floor = 1.0 / (float(n_perm) + 1.0)
    if floor > bar:
        raise ValueError(
            f"n_perm={n_perm} cannot resolve the rank-1 Benjamini-Hochberg bar "
            f"{bar:g} for a family of {family_size} (smallest reachable p is "
            f"{floor:g}); raise --n-perm-mde to at least "
            f"{int(math.ceil(1.0 / bar))}")
    if vals.size == 0 or int(mask.sum()) in (0, vals.size):
        return None

    def _p_at(d: float) -> Optional[float]:
        shifted = vals - np.where(mask, float(d), 0.0)
        if cluster:
            return cluster_permutation_pvalue_group_diff(
                trades, shifted, mask, n_perm=n_perm, seed=seed).get("p")
        return permutation_pvalue_group_diff(shifted, mask, n_perm=n_perm,
                                             seed=seed)

    coarse = None
    steps = int(round(float(grid_max) / float(grid_step)))
    for i in range(steps + 1):
        d = round(i * float(grid_step), 9)
        p = _p_at(d)
        if p is not None and p <= bar:
            coarse = d
            break
    if coarse is None:
        return None
    if coarse == 0.0:
        return 0.0
    lo = max(0.0, coarse - float(grid_step))
    fine = coarse
    n_fine = int(round(float(grid_step) / float(refine_step)))
    for i in range(n_fine + 1):
        d = round(lo + i * float(refine_step), 9)
        p = _p_at(d)
        if p is not None and p <= bar:
            fine = d
            break
    return round(float(fine), 6)


def min_detectable_effect_eff(trades, values, suppressed, family_size, **kw):
    """Detection limit on the PRIMARY target, in efficiency units."""
    return min_detectable_effect_on_grid(
        trades, values, suppressed, family_size,
        grid_step=MDE_EFF_GRID_STEP, grid_max=MDE_EFF_GRID_MAX,
        refine_step=MDE_EFF_REFINE_STEP, **kw)


def min_detectable_effect_pp(trades, values, suppressed, family_size, **kw):
    """Detection limit on the CONTINUITY target, in pp of net return."""
    return min_detectable_effect_on_grid(
        trades, values, suppressed, family_size,
        grid_step=MDE_PP_GRID_STEP, grid_max=MDE_PP_GRID_MAX,
        refine_step=MDE_PP_REFINE_STEP, **kw)


def protocol_verdict(windows: dict, protocol_names: Sequence[str],
                     min_windows: int) -> tuple:
    """(passed, n_holding, n_with_legs, reasons) for the protocol-window rule.

    A protocol window HOLDS when its mean drawdown magnitude falls, its chop
    loss falls, and the return give-up stays inside the tolerance. The rule
    needs ``min_windows`` of the windows that CARRY LEGS to hold, and at least
    ``min_windows`` of them to carry legs at all — otherwise the check is
    untestable and fails closed rather than passing on a thin sample.
    """
    reasons = []
    with_legs = [n for n in protocol_names
                 if (windows.get(n) or {}).get("n_legs")]
    holding = 0
    for name in protocol_names:
        row = windows.get(name)
        if not row or not row.get("n_legs"):
            reasons.append(f"{name}: no legs")
            continue
        own = []
        if not (row["dd_delta"] < 0):
            own.append(f"drawdown not reduced ({row['dd_delta']:+.2f} pp)")
        if not (row["chop_delta"] < 0):
            own.append(f"chop loss not reduced ({row['chop_delta']:+.2f} pp)")
        tol = max(RETURN_TOLERANCE_PP,
                  RETURN_TOLERANCE_FRAC * abs(row["ret_ungated"]))
        if not (row["ret_gated"] >= row["ret_ungated"] - tol):
            own.append(
                f"return give-up {row['ret_ungated'] - row['ret_gated']:.2f} pp "
                f"exceeds tolerance {tol:.2f} pp")
        if own:
            reasons.append(f"{name}: " + ", ".join(own))
        else:
            holding += 1
    if len(with_legs) < int(min_windows):
        reasons.append(
            f"only {len(with_legs)} protocol windows carry legs "
            f"(need {int(min_windows)})")
        return False, holding, len(with_legs), reasons
    if holding < int(min_windows):
        reasons.append(
            f"holds on only {holding}/{len(with_legs)} protocol windows with "
            f"legs (need {int(min_windows)})")
        return False, holding, len(with_legs), reasons
    # Every failure the loop above recorded is subsumed by the count rule the
    # config just cleared, so a passing config carries no reasons.
    return True, holding, len(with_legs), []


def held_out_verdict(windows: dict, held_out_names: Sequence[str]) -> tuple:
    """#1422's held-out drawdown rule, unchanged: 2/3 of the leg-carrying
    windows must not degrade, with at least ``HELD_OUT_MIN_WINDOWS`` of them."""
    return study1422.held_out_verdict(windows, held_out_names)


def config_verdict(cfg: dict) -> tuple:
    """Pure accept/reject for one swept config. Returns ``(passed, reasons)``.

    #1422's rule shape with two deliberate changes, both pre-registered:

      * SIGNIFICANCE reads the cluster p on the PRIMARY TARGET (efficiency).
        That is Route 3: the mechanism question is asked on the statistic the
        design can actually resolve.
      * ECONOMICS still read NET RETURN, on the 3-of-4 protocol rule and the
        unchanged held-out rule. A bounded-variance win is mechanism evidence
        and never on its own a licence to ship a gate — the money has to be
        there too.

    An untestable cluster p (None) fails closed, exactly as in #1422.
    """
    reasons = []
    if float(cfg.get("n_suppressed_effective") or 0.0) < MIN_SUPPRESSED_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_suppressed_effective'), 1)} effective suppressed "
            f"trades (floor {MIN_SUPPRESSED_EFFECTIVE:g})")
    if float(cfg.get("n_kept_effective") or 0.0) < MIN_KEPT_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_kept_effective'), 1)} effective kept trades "
            f"(floor {MIN_KEPT_EFFECTIVE:g})")
    if cfg.get("p_cluster") is None:
        reasons.append("cluster permutation p on the primary target is "
                       f"untestable ({cfg.get('cluster_reason') or 'no draws'})")
    elif not cfg.get("bh_reject"):
        reasons.append(
            f"not significant after Benjamini-Hochberg on the primary target's "
            f"cluster p (cluster p={cfg.get('p_cluster')})")
    windows = cfg.get("windows") or {}
    ok_proto, _holding, _with_legs, proto_reasons = protocol_verdict(
        windows, cfg.get("protocol_windows") or (),
        int(cfg.get("protocol_min_windows") or 0))
    if not ok_proto:
        reasons.extend(proto_reasons)
    ok, non_deg, n_with = held_out_verdict(
        windows, cfg.get("held_out_windows") or ())
    if not ok:
        reasons.append(
            f"drawdown holds on only {non_deg}/{n_with} held-out windows with legs "
            f"(need {HELD_OUT_MIN_FRACTION:.2f} of them, min {HELD_OUT_MIN_WINDOWS} "
            f"windows)")
    return (not reasons), reasons


def protocol_dd_reduction(cfg: dict) -> float:
    """Pooled protocol-window drawdown reduction, positive = better."""
    windows = cfg.get("windows") or {}
    total = 0.0
    for name in cfg.get("protocol_windows") or ():
        row = windows.get(name)
        if row and row.get("n_legs"):
            total += -float(row["dd_delta"])
    return round(total, 6)


def validity_gate(mde: dict) -> dict:
    """The pre-registered validity gate, in efficiency units.

    This study is VALID only when the measured cluster-null detection limit on
    the primary cohort falls BELOW that same cohort's pool-matched observed
    separation on the same statistic. Both numbers come from the SAME rows —
    reading a whole-study separation against a sub-cohort's limit compares two
    different samples and is the exact mistake #1422's pool-matched table exists
    to prevent.

    When the gate fails, no null this study measures is a statement about the
    market, and the Recommendation must say so.
    """
    limit = mde.get("pooled_primary_cluster")
    seps = ((mde.get("observed_separation_by_pool") or {}).get("primary") or {})
    largest = (max(abs(float(v)) for v in seps.values()) if seps else None)
    if limit is None or largest is None:
        return {
            "passed": False,
            "limit": limit,
            "largest_separation": largest,
            "reason": ("the primary cohort's detection limit is above "
                       f"{MDE_EFF_GRID_MAX:g} efficiency units, so no effect on "
                       f"the injection grid is resolvable"
                       if largest is not None else
                       "the primary cohort carries no measurable separation"),
        }
    return {
        "passed": bool(largest >= limit),
        "limit": round(float(limit), 6),
        "largest_separation": round(float(largest), 6),
        "reason": "",
    }


def decide_recommendation(configs: Sequence[dict], mde: dict) -> dict:
    """Mechanically derive the Recommendation.

    ONLY primary-cohort configs can win. The exploratory grid is reported for
    completeness and can never produce a recommendation, because its hypotheses
    were selected by reading the same data they are scored on.
    """
    gate = validity_gate(mde)
    primary = [c for c in configs if c.get("cohort") == COHORT_PRIMARY]
    families = {}
    for family in FAMILIES:
        own = [c for c in primary if c.get("family") == family]
        passing = [c for c in own if config_verdict(c)[0]]
        if passing:
            best = sorted(passing,
                          key=lambda c: (-protocol_dd_reduction(c), c["config_id"]))[0]
            families[family] = {"winner": best, "n_tested": len(own),
                                "n_passing": len(passing)}
        else:
            families[family] = {"winner": None, "n_tested": len(own),
                                "n_passing": 0}
    if any(v["winner"] for v in families.values()):
        return {"verdict": VERDICT_CONFIG, "families": families,
                "validity_gate": gate, "key_risk_held": False,
                "justification": ""}

    tested = sum(v["n_tested"] for v in families.values())
    n_significant = sum(1 for c in primary if c.get("bh_reject"))
    n_untestable = sum(1 for c in primary if c.get("p_cluster") is None)
    best = None
    for cfg in primary:
        p = cfg.get("p_cluster")
        if p is not None and (best is None or p < best[0]):
            best = (p, cfg["config_id"])
    best_text = (f" The primary hypothesis reached cluster p={best[0]:.4f} "
                 f"(`{best[1]}`) on the primary target." if best else "")

    # The KEY RISK #1424 was required to state before the run: a
    # better-powered design may simply confirm the null, and that outcome is
    # the ANSWER rather than a request for more data. It only "held" when the
    # design could actually see an effect the size its own buckets show.
    key_risk_held = bool(gate["passed"])

    if gate["passed"]:
        detail = (
            f"The validity gate PASSED: the primary cohort's detection limit is "
            f"{gate['limit']:.3f} efficiency units and that same cohort separates "
            f"by {gate['largest_separation']:.3f}, so an effect of the size these "
            f"buckets show IS resolvable here. The null is therefore a statement "
            f"about the market and not about the design: there is NO usable Hurst "
            f"edge at or above this limit, and the pre-registered key risk — that "
            f"the visible separations reflect whole datasets moving together "
            f"rather than H sorting trades within them — is what the run found. "
            f"This closes the question rather than licensing a fourth study.")
    else:
        detail = (
            f"The validity gate FAILED: "
            + (gate["reason"] or
               f"the primary cohort's detection limit is "
               f"{gate['limit']:.3f} efficiency units while that cohort separates "
               f"by only {gate['largest_separation']:.3f}, BELOW the limit")
            + ". A separation under the limit is INVISIBLE to this design, so "
              "this null bounds the edge from above and says nothing either way "
              "about a smaller one. The verdict stays a POWER statement. Do not "
              "read it as evidence that no edge exists.")

    return {
        # The verdict WORD is itself mechanical. A null measured by a design
        # that could see the effect its buckets show is a RESOLVED NULL and an
        # answer; the same null under a design that could not is merely
        # inconclusive. Printing one word for both is what made #1410's and
        # #1422's outcomes look alike when they were not.
        "verdict": VERDICT_RESOLVED_NULL if gate["passed"] else VERDICT_INCONCLUSIVE,
        "families": families,
        "validity_gate": gate,
        "key_risk_held": key_risk_held,
        "justification": (
            f"No configuration of the {tested} primary hypothesis "
            f"{'set' if tested == 1 else 'hypotheses'} passed the pre-registered "
            f"acceptance rule. {n_significant} reached Benjamini-Hochberg "
            f"significance on the cluster permutation at alpha={ALPHA}; "
            f"{n_untestable} were untestable.{best_text} {detail}"
        ).strip(),
    }


# ---------------------------------------------------------------------------
# Coverage, correlations.
# ---------------------------------------------------------------------------

def owned_windows(dataset: tuple, window_names: Sequence[str]) -> list:
    """The windows this dataset OWNS, intersected with the requested set."""
    owned = DATASET_WINDOWS.get(tuple(dataset)) or ()
    return [w for w in window_names if w in owned]


def coverage_audit(frames: dict, window_names: Sequence[str],
                   hurst_windows: Sequence[int]) -> dict:
    """Which (dataset, window) cells the cache can actually support.

    #1422's audit with one addition: a dataset is only ever measured against
    the windows it OWNS. A cell no venue owns is not "dropped for lack of
    data" — it was never in the design, and reporting it as a drop would bury
    the real drops (a late listing, a delisting gap) under dozens of
    by-construction absences.
    """
    need_lead = max((required_lead_bars(hw) for hw in hurst_windows), default=0)
    last_bars = [f.index[-1] for f in frames.values()
                 if f is not None and not f.empty]
    reference_last = max(last_bars) if last_bars else None
    cells = {}
    dropped = []
    n_unowned = 0
    for dataset, frame in sorted(frames.items()):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        own = owned_windows(dataset, window_names)
        n_unowned += len(window_names) - len(own)
        for wname in own:
            start, end = WINDOWS[wname]
            ok = True
            reason = ""
            if frame is None or frame.empty:
                ok, reason = False, "no cached bars"
            else:
                lead = warmup_lead_bars(frame.index, pd.Timestamp(start))
                in_window = len(slice_window(frame, WINDOWS[wname]))
                want = expected_bars(WINDOWS[wname], timeframe, reference_last)
                if lead < need_lead:
                    ok = False
                    reason = (f"lead {lead} bars before {start} < required "
                              f"{need_lead}")
                elif in_window <= 0:
                    ok = False
                    reason = f"no bars inside [{start}, {end or 'latest'})"
                elif want > 0 and in_window < MIN_WINDOW_BAR_FRACTION * want:
                    ok = False
                    reason = (f"only {in_window} of ~{want} expected bars inside "
                              f"[{start}, {end or 'latest'}) "
                              f"({in_window / want:.0%} < "
                              f"{MIN_WINDOW_BAR_FRACTION:.0%}) — data gap")
            cells[f"{key}|{wname}"] = ok
            if not ok:
                dropped.append({"dataset": key, "window": wname, "reason": reason})
    return {
        "required_lead_bars": need_lead,
        "min_window_bar_fraction": MIN_WINDOW_BAR_FRACTION,
        "reference_last_bar": None if reference_last is None else str(reference_last),
        "n_cells": len(cells),
        "n_kept": int(sum(1 for v in cells.values() if v)),
        "n_dropped": len(dropped),
        "n_unowned": n_unowned,
        "cells": cells,
        "dropped": dropped,
    }


def scored_warmup_leads(frames: dict, coverage: dict,
                        scored_windows: Sequence[str]) -> dict:
    """Warm-up lead per dataset, measured against the windows IT ACTUALLY
    SCORES — #1422's rule, on venue-qualified keys."""
    leads = {}
    for dataset, frame in frames.items():
        if frame is None or frame.empty:
            continue
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        own = [w for w in scored_windows
               if (coverage.get("cells") or {}).get(f"{key}|{w}")]
        if not own:
            continue
        own_first = min(pd.Timestamp(WINDOWS[w][0]) for w in own)
        leads[key] = warmup_lead_bars(frame.index, own_first)
    return leads


def symbol_return_correlations(frames: dict) -> dict:
    """Daily-log-return correlation between VENUE-QUALIFIED symbols.

    Same shape as #1422's, with one pre-registered override: two symbols
    sharing a BASE ASSET take 1.0, whatever their quote currency or venue.
    ``BTC/USD`` on Bitstamp and ``BTC/USDT`` on Binance.US are one market, so
    crediting them a measured correlation below 1 — or, worse, no measurement
    at all where their spans do not overlap — would hand the effective-N
    estimator independence that does not exist. The estimator's own default for
    an unknown pair is already 1.0; this makes the same choice explicit and
    testable rather than incidental.
    """
    finest = {}
    for dataset, frame in sorted(frames.items()):
        if frame is None or frame.empty:
            continue
        exchange_id, symbol, timeframe = dataset
        qsym = qualified_symbol(exchange_id, symbol)
        rank = (timeframe_minutes(timeframe), timeframe)
        current = finest.get(qsym)
        if current is None or rank < current[0]:
            finest[qsym] = (rank, frame)

    daily = {}
    for qsym in sorted(finest):
        closes = finest[qsym][1]["close"].astype(float)
        d = closes.resample("1D").last().dropna()
        if len(d) < 30:
            continue
        daily[qsym] = np.log(d).diff().dropna()

    out = {}
    syms = sorted(finest)
    for i, a in enumerate(syms):
        for b in syms[i + 1:]:
            if base_asset(a) == base_asset(b):
                out[(a, b)] = 1.0
                continue
            if a not in daily or b not in daily:
                continue
            joined = pd.concat([daily[a], daily[b]], axis=1, join="inner").dropna()
            if len(joined) < 30:
                continue
            if (joined.iloc[:, 0].nunique() < 2
                    or joined.iloc[:, 1].nunique() < 2):
                continue
            rho = float(joined.iloc[:, 0].corr(joined.iloc[:, 1]))
            if math.isfinite(rho):
                out[(a, b)] = round(rho, 6)
    return out


# ---------------------------------------------------------------------------
# Engine arms.
# ---------------------------------------------------------------------------

def trade_samples_with_side(results: dict) -> list:
    """``trade_samples_with_span`` plus the fill SIDE.

    The primary target is SIGNED, so it needs the direction the fill took. The
    shared helper does not carry it; every other field is produced by that
    helper so the two can never diverge.
    """
    sides = [t.get("side") for t in (results.get("trades") or [])]
    out = trade_samples_with_span(results)
    if len(sides) != len(out):
        raise AssertionError(
            f"trade_samples_with_span returned {len(out)} rows for "
            f"{len(sides)} trades")
    for sample, side in zip(out, sides):
        sample["side"] = side
    return out


def _run_arm(reg, name: str, symbol: str, timeframe: str, df: pd.DataFrame,
             armed, overrides: dict) -> Optional[dict]:
    """One Backtester run on a pre-sliced frame, optionally entry-masked.

    #1422's arm verbatim, re-implemented only so the leg carries the fill side
    the primary target needs. Kwargs mirror ``eval_windows.run_leg``'s
    no-regime, no-profile path exactly; every leg additionally verifies its
    ungated arm against ``run_leg`` itself, so the mirror cannot drift silently.
    """
    from atr import ensure_atr_indicator
    from backtester import Backtester
    from run_backtest import FUNDING_COLUMN_STRATEGIES

    if name in FUNDING_COLUMN_STRATEGIES:
        raise ValueError(
            f"{name} needs the funding column; this study's exemplars must not")
    if df.empty:
        return None

    strat = reg.STRATEGY_REGISTRY.get(name)
    if strat is None:
        raise SystemExit(f"Unknown strategy {name!r}")
    strat_params = strat["default_params"]
    close_strategies = overrides.get("close_strategies")

    df_signals = reg.apply_strategy(name, df, strat_params)
    if armed is not None:
        df_signals = df_signals.copy()
        df_signals["signal"] = study1410.mask_entry_signals(
            df_signals["signal"].fillna(0).to_numpy(), armed)
    if close_strategies:
        df_signals = ensure_atr_indicator(df_signals)

    bt = Backtester(
        initial_capital=DEFAULT_CAPITAL, platform=FEE_PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=None, invert_signal=False,
        stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=overrides.get("trailing_stop_atr_mult"),
        profile_allocation=None,
        regime_enabled=False,
        regime_period=14,
        regime_adx_threshold=20.0,
        allowed_regimes=None,
        regime_windows_spec=None,
        commission_pct=None,
        intrabar_resolution="ohlc_walk",
    )
    results = bt.run(df_signals, strategy_name=name, symbol=symbol,
                     timeframe=timeframe, params=strat_params, save=False)
    leg = leg_from_results(results)
    leg["trade_samples"] = trade_samples_with_side(results)
    return leg


def _leg_metrics(leg: Optional[dict]) -> dict:
    if leg is None:
        return {"return_pct": 0.0, "max_dd_pct": 0.0, "chop_loss": 0.0, "trades": 0}
    rets = [s["pnl_pct_net"] for s in leg.get("trade_samples") or []]
    return {
        "return_pct": float(leg["return_pct"]),
        "max_dd_pct": float(leg["max_dd_pct"]),
        "chop_loss": chop_loss(rets),
        "trades": int(leg["trades"]),
    }


def build_leg(reg, family: str, exemplar: str, dataset: tuple,
              window_name: str, full: pd.DataFrame, hurst_by_window: dict,
              adx_stamp: pd.Series, verify_mirror: bool = True) -> Optional[dict]:
    """Every arm for one (exemplar, dataset, window) cell.

    #1422's ``build_leg`` plus the venue-qualified identity and the per-trade
    primary-target stamp.
    """
    exchange_id, symbol, timeframe = dataset
    qsym = qualified_symbol(exchange_id, symbol)
    window = WINDOWS[window_name]
    overrides = EXEMPLAR_CLOSE_OVERRIDES.get(exemplar, {})
    df = slice_window(full, window)
    if df.empty:
        return None
    sense = FAMILY_SENSE[family]

    ungated = _run_arm(reg, exemplar, symbol, timeframe, df, None, overrides)
    if ungated is None:
        return None

    mirror_ok = None
    if verify_mirror:
        reference = run_leg(reg, exemplar, None, symbol, timeframe, window,
                            capital=DEFAULT_CAPITAL,
                            close_strategies=overrides.get("close_strategies"),
                            stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
                            trailing_stop_atr_mult=overrides.get("trailing_stop_atr_mult"),
                            keep_trades=True,
                            exchange_id=exchange_id)
        mirror_ok = reference is not None and all(
            reference.get(k) == ungated.get(k) for k in _MIRRORED_LEG_KEYS)
        if not mirror_ok:
            raise AssertionError(
                f"gated-arm mirror diverged from eval_windows.run_leg for "
                f"{exemplar} {qsym} {timeframe} {window_name}: "
                f"{ {k: (reference or {}).get(k) for k in _MIRRORED_LEG_KEYS} } vs "
                f"{ {k: ungated.get(k) for k in _MIRRORED_LEG_KEYS} }")

    index_keys = [str(ts) for ts in df.index]
    key_pos = {k: i for i, k in enumerate(index_keys)}
    closes = df["close"].to_numpy(dtype=float)
    k_bars = horizon_bars(timeframe)

    stamps = {}
    decisions = {}
    for hw, rolling in hurst_by_window.items():
        stamps[hw] = entry_stamp_series(rolling).reindex(df.index).to_numpy(dtype=float)
        decisions[hw] = decision_series(rolling).reindex(df.index).to_numpy(dtype=float)
    adx_vals = adx_stamp.reindex(df.index).to_numpy(dtype=float)

    armed_signal_bar = {}
    armed_fill_bar = {}
    for hw in hurst_by_window:
        for arm, disarm in GATE_PAIRS[family]:
            cid = gate_config_id(family, hw, arm, disarm)
            mask = hysteresis_mask(decisions[hw], arm, disarm, sense)
            armed_signal_bar[cid] = mask
            shifted = np.empty_like(mask)
            shifted[0] = bool(GATE_INITIAL_ARMED)
            shifted[1:] = mask[:-1]
            armed_fill_bar[cid] = shifted

    cohort = cell_cohort(exchange_id, symbol, timeframe, window_name)
    trades = []
    n_horizon_excluded = 0
    for sample in ungated.get("trade_samples") or []:
        key = str(sample["entry_date"])
        pos = key_pos.get(key)
        if pos is None:
            raise AssertionError(
                f"trade entry_date {key!r} is not a bar of the {window_name} slice "
                f"for {exemplar} {qsym} {timeframe}")
        try:
            exit_ns = int(pd.Timestamp(sample["exit_date"]).value)
        except (ValueError, TypeError):
            exit_ns = None
        eff = signed_efficiency(closes, pos, k_bars,
                                trade_direction(sample.get("side")))
        if eff is None:
            n_horizon_excluded += 1
        trades.append({
            "strategy": exemplar,
            "exchange": exchange_id,
            "symbol": qsym,
            "base_symbol": symbol,
            "timeframe": timeframe,
            "window": window_name,
            "cohort": cohort,
            "entry_date": key,
            "entry_ns": int(pd.Timestamp(key).value),
            "exit_ns": exit_ns,
            "pnl_pct_net": float(sample["pnl_pct_net"]),
            "efficiency": None if eff is None else round(float(eff), 6),
            "adx": (None if not math.isfinite(adx_vals[pos])
                    else float(adx_vals[pos])),
            "h": {hw: (None if not math.isfinite(stamps[hw][pos])
                       else float(stamps[hw][pos])) for hw in hurst_by_window},
            "armed": {cid: bool(armed_fill_bar[cid][pos]) for cid in armed_fill_bar},
        })

    gated = {}
    for cid, mask in armed_signal_bar.items():
        arm_leg = _run_arm(reg, exemplar, symbol, timeframe, df, mask, overrides)
        gated[cid] = _leg_metrics(arm_leg)

    return {
        "family": family,
        "strategy": exemplar,
        "exchange": exchange_id,
        "symbol": qsym,
        "base_symbol": symbol,
        "timeframe": timeframe,
        "dataset": dataset_key(qsym, timeframe),
        "window": window_name,
        "cohort": cohort,
        "bars": int(len(df)),
        "horizon_bars": k_bars,
        "n_horizon_excluded": n_horizon_excluded,
        "mirror_verified": mirror_ok,
        "ungated": _leg_metrics(ungated),
        "gated": gated,
        "trades": trades,
    }


# ---------------------------------------------------------------------------
# Aggregation.
# ---------------------------------------------------------------------------

def bucket_tables(trades: Sequence[dict], hurst_window: int) -> dict:
    """Part A: per-bucket outcome table over an already-deduped trade pool.

    #1422's table plus the PRIMARY TARGET's mean and its row count, so the
    separation the validity gate adjudicates is visible in the same table the
    economics are read from.
    """
    by_bucket = {b: {"ret": [], "eff": []} for b in BUCKETS}
    for t in trades:
        row = by_bucket[bucket_label((t.get("h") or {}).get(hurst_window))]
        row["ret"].append(t["pnl_pct_net"])
        if t.get("efficiency") is not None:
            row["eff"].append(float(t["efficiency"]))
    out = {}
    for bucket, vals in by_bucket.items():
        rets = vals["ret"]
        effs = vals["eff"]
        total_return, max_dd = compound_equity(rets)
        out[bucket] = {
            "trades": len(rets),
            "win_rate_pct": win_rate(rets),
            "mean_pnl_pct_net": round(float(np.mean(rets)), 6) if rets else None,
            "median_pnl_pct_net": round(float(np.median(rets)), 6) if rets else None,
            "compounded_return_pct": total_return,
            "trade_seq_max_dd_pct": max_dd,
            "chop_loss_pct": chop_loss(rets),
            "efficiency_trades": len(effs),
            "mean_efficiency": round(float(np.mean(effs)), 6) if effs else None,
        }
    return out


def joint_adx_hurst_table(trades: Sequence[dict], hurst_window: int) -> dict:
    """Part D: the ADX-level x Hurst-level cell table for one family."""
    return study1422.joint_adx_hurst_table(trades, hurst_window)


def joint_separation_verdict(trades: Sequence[dict], hurst_window: int,
                             n_perm: int = N_PERM, seed: int = SEED) -> dict:
    """#1412 Stage 0, on NET RETURN and on the expanded pool.

    Deliberately NOT moved to the primary target. Stage 0 asks an ECONOMIC
    question — is high-ADX + low-H materially worse for these entries — and
    #1422 discharged it on net return. Re-running it on a different statistic
    would produce a verdict that cannot be compared with the one already
    recorded against #1412.
    """
    return study1422.joint_separation_verdict(trades, hurst_window,
                                              n_perm=n_perm, seed=seed)


def _cohort_legs(legs: Sequence[dict], family: str, cohort: str) -> list:
    return [lg for lg in legs
            if lg["family"] == family and lg["cohort"] == cohort]


def _window_rows_gate(legs: Sequence[dict], family: str, cohort: str,
                      config_id: str) -> dict:
    """Per-window mean deltas for a gate config, over that cohort's legs only."""
    rows = {}
    own = _cohort_legs(legs, family, cohort)
    for wname in WINDOW_ORDER:
        cells = [lg for lg in own
                 if lg["window"] == wname and config_id in lg["gated"]]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u, trades_g, trades_u = [], [], [], [], [], []
        for lg in cells:
            u, g = lg["ungated"], lg["gated"][config_id]
            dd_deltas.append(abs(g["max_dd_pct"]) - abs(u["max_dd_pct"]))
            chop_deltas.append(g["chop_loss"] - u["chop_loss"])
            ret_g.append(g["return_pct"])
            ret_u.append(u["return_pct"])
            trades_g.append(g["trades"])
            trades_u.append(u["trades"])
        rows[wname] = {
            "n_legs": len(cells),
            "dd_delta": round(float(np.mean(dd_deltas)), 6),
            "chop_delta": round(float(np.mean(chop_deltas)), 6),
            "ret_gated": round(float(np.mean(ret_g)), 6),
            "ret_ungated": round(float(np.mean(ret_u)), 6),
            "trades_gated": int(sum(trades_g)),
            "trades_ungated": int(sum(trades_u)),
        }
    return rows


def _window_rows_size(legs: Sequence[dict], family: str, cohort: str,
                      hurst_window: int, gain: float) -> dict:
    """Per-window mean deltas for a sizing config, over that cohort's legs only."""
    sense = FAMILY_SENSE[family]
    rows = {}
    own = _cohort_legs(legs, family, cohort)
    for wname in WINDOW_ORDER:
        cells = [lg for lg in own if lg["window"] == wname]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u = [], [], [], []
        n_used = 0
        for lg in cells:
            rets = [t["pnl_pct_net"] for t in lg["trades"]]
            if not rets:
                continue
            mults = [size_multiplier((t.get("h") or {}).get(hurst_window), sense, gain)
                     for t in lg["trades"]]
            base_ret, base_dd = compound_equity(rets)
            sized_ret, sized_dd = compound_equity(rets, mults)
            dd_deltas.append(abs(sized_dd) - abs(base_dd))
            chop_deltas.append(
                chop_loss([m * r for m, r in zip(mults, rets)]) - chop_loss(rets))
            ret_g.append(sized_ret)
            ret_u.append(base_ret)
            n_used += 1
        if not n_used:
            rows[wname] = {"n_legs": 0}
            continue
        rows[wname] = {
            "n_legs": n_used,
            "dd_delta": round(float(np.mean(dd_deltas)), 6),
            "chop_delta": round(float(np.mean(chop_deltas)), 6),
            "ret_gated": round(float(np.mean(ret_g)), 6),
            "ret_ungated": round(float(np.mean(ret_u)), 6),
            "trades_gated": int(sum(len(lg["trades"]) for lg in cells)),
            "trades_ungated": int(sum(len(lg["trades"]) for lg in cells)),
        }
    return rows


def _config_shell(family: str, cohort: str, mode: str, hw: int,
                  arm=None, disarm=None, gain=None) -> dict:
    cid = (gate_config_id(family, hw, arm, disarm) if mode == "gate"
           else size_config_id(family, hw, gain))
    if cohort == COHORT_PRIMARY:
        protocol = PRIMARY_PROTOCOL_WINDOWS
        protocol_min = PRIMARY_PROTOCOL_MIN_WINDOWS
        held_out = PRIMARY_HELD_OUT_WINDOWS
    else:
        protocol = EXPLORATORY_PROTOCOL_WINDOWS
        protocol_min = EXPLORATORY_PROTOCOL_MIN_WINDOWS
        held_out = EXPLORATORY_HELD_OUT_WINDOWS
    return {
        "config_id": cid,
        "cohort": cohort,
        "family": family,
        "mode": mode,
        "sense": FAMILY_SENSE[family],
        "hurst_window": hw,
        "arm": arm,
        "disarm": disarm,
        "gain": gain,
        "protocol_windows": list(protocol),
        "protocol_min_windows": protocol_min,
        "held_out_windows": list(held_out),
    }


def _sweep_grid(cohort: str, hurst_windows: Sequence[int]) -> list:
    """(family, mode, hw, arm, disarm, gain) tuples this cohort tests.

    PRIMARY tests the ONE pinned hypothesis; EXPLORATORY tests the full #1410
    grid. The two lists are disjoint by construction, which is what keeps their
    Benjamini-Hochberg denominators from ever merging — and the primary
    denominator of exactly 1 is Route 1's entire dividend.
    """
    grid = []
    for family in FAMILIES:
        for hw in hurst_windows:
            for arm, disarm in GATE_PAIRS[family]:
                cid = gate_config_id(family, hw, arm, disarm)
                if cohort == COHORT_PRIMARY and cid not in PRIMARY_CONFIG_IDS:
                    continue
                grid.append((family, "gate", hw, arm, disarm, None))
            for gain in SIZING_GAINS:
                cid = size_config_id(family, hw, gain)
                if cohort == COHORT_PRIMARY and cid not in PRIMARY_CONFIG_IDS:
                    continue
                grid.append((family, "size", hw, None, None, gain))
    return grid


def _target_rows(trades: Sequence[dict]) -> tuple:
    """``(rows, n_missing)`` — rows carrying a defined primary target.

    Applied BEFORE the cluster-usability filter, because dropping
    horizon-truncated rows can shorten a dataset's scored span and therefore
    change whether that dataset can host a rotation at all. Doing it the other
    way round would decide rotatability on rows the contrast never scores.
    """
    rows = [t for t in trades if t.get("efficiency") is not None]
    return rows, len(trades) - len(rows)


def build_configs(legs: Sequence[dict], pooled: dict, hurst_windows: Sequence[int],
                  rho_by_symbol: dict, n_perm: int, seed: int) -> list:
    """Every swept config in both cohorts, on both targets, with economics.

    Both targets are scored on ONE row set — the rows carrying a defined
    primary target that the cluster null can also rotate. A continuity p-value
    measured on a different sample than the verdict's p-value would describe a
    different study.
    """
    configs = []
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for family, mode, hw, arm, disarm, gain in _sweep_grid(cohort, hurst_windows):
            sense = FAMILY_SENSE[family]
            trades = [t for t in (pooled.get(family) or [])
                      if t["cohort"] == cohort]
            cfg = _config_shell(family, cohort, mode, hw, arm, disarm, gain)
            cid = cfg["config_id"]
            if mode == "gate":
                sub = [t for t in trades if cid in t["armed"]]
            else:
                sub = list(trades)
            sub, n_missing_target = _target_rows(sub)
            idx, excluded = usable_cluster_rows(sub)
            n_excluded = len(sub) - len(idx)
            sub = [sub[i] for i in idx]
            values = [float(t["efficiency"]) for t in sub]
            returns = [float(t["pnl_pct_net"]) for t in sub]
            if mode == "gate":
                suppressed = [not t["armed"][cid] for t in sub]
                cfg["p_raw"] = permutation_pvalue_group_diff(
                    values, suppressed, n_perm=n_perm, seed=seed)
                cluster = cluster_permutation_pvalue_group_diff(
                    sub, values, suppressed, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = permutation_pvalue_group_diff(
                    returns, suppressed, n_perm=n_perm, seed=seed)
                cfg["p_cluster_return"] = cluster_permutation_pvalue_group_diff(
                    sub, returns, suppressed, n_perm=n_perm, seed=seed).get("p")
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg["windows"] = _window_rows_gate(legs, family, cohort, cid)
            else:
                mults = [size_multiplier((t.get("h") or {}).get(hw), sense, gain)
                         for t in sub]
                cfg["p_raw"] = permutation_pvalue_weighted(
                    values, mults, n_perm=n_perm, seed=seed)
                cluster = cluster_permutation_pvalue_weighted(
                    sub, values, mults, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = permutation_pvalue_weighted(
                    returns, mults, n_perm=n_perm, seed=seed)
                cfg["p_cluster_return"] = cluster_permutation_pvalue_weighted(
                    sub, returns, mults, n_perm=n_perm, seed=seed).get("p")
                # A sizing config's "suppressed" analogue is the down-weighted
                # side (m < 1); the same volume floors then apply unchanged.
                sup_rows = [t for t, m in zip(sub, mults) if m < 1.0]
                kept_rows = [t for t, m in zip(sub, mults) if m >= 1.0]
                cfg["windows"] = _window_rows_size(legs, family, cohort, hw, gain)
            cfg["p_cluster"] = cluster.get("p")
            cfg["cluster_draws"] = cluster.get("n_draws")
            cfg["cluster_excluded_datasets"] = excluded
            cfg["cluster_excluded_trades"] = n_excluded
            cfg["cluster_offset_range"] = cluster.get("offset_range")
            cfg["cluster_distinct_offsets"] = cluster.get("n_distinct_offsets")
            cfg["cluster_reason"] = cluster.get("reason")
            cfg["n_pooled_trades"] = len(sub)
            cfg["n_missing_target"] = n_missing_target
            cfg["n_suppressed"] = len(sup_rows)
            cfg["n_kept"] = len(kept_rows)
            cfg["n_pooled_effective"] = effective_n(sub, rho_by_symbol)
            cfg["n_suppressed_effective"] = effective_n(sup_rows, rho_by_symbol)
            cfg["n_kept_effective"] = effective_n(kept_rows, rho_by_symbol)
            configs.append(cfg)
    return configs


def apply_bh_by_cohort(configs: Sequence[dict], alpha: float = ALPHA) -> None:
    """Benjamini-Hochberg over the PRIMARY TARGET's cluster p, per cohort.

    The primary and exploratory families never share a denominator. With one
    pre-registered primary hypothesis the primary denominator is 1, so its
    rank-1 bar is alpha rather than alpha/4 — Route 1, applied.
    """
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        own = [c for c in configs if c.get("cohort") == cohort]
        # RESET, never setdefault: a stale True from an earlier pass must not
        # survive a correction that would not grant it.
        for cfg in own:
            cfg["bh_reject"] = False
        testable = [c for c in own if c.get("p_cluster") is not None]
        if not testable:
            continue
        flags = benjamini_hochberg([c["p_cluster"] for c in testable], alpha=alpha,
                                   family_size=len(own))
        for cfg, flag in zip(testable, flags):
            cfg["bh_reject"] = bool(flag)


# ---------------------------------------------------------------------------
# Detection limits.
# ---------------------------------------------------------------------------

def measure_detection_limits(pooled: dict, hurst_windows: Sequence[int],
                             n_perm: int, seed: int) -> dict:
    """What this design could have detected, on BOTH targets.

    Three pools under one injection model: #1410's own cells (30 hypotheses),
    this study's primary cohort (ONE hypothesis), and its exploratory grid
    (30). Each pool's separations, limits and zero-injection p-values are
    measured on the SAME rows, so the validity gate compares like with like.
    """
    out: dict = {"by_family_cluster": {}, "by_family_cluster_return": {}}
    hw = max(hurst_windows)

    def _pool(family: str, cohort: Optional[str], only_1410_cells: bool):
        rows = []
        for t in pooled.get(family) or []:
            if cohort is not None and t["cohort"] != cohort:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if only_1410_cells and key not in D_1410:
                continue
            rows.append(t)
        return rows

    def _split(rows, family: str):
        # The suppressed side is the FAMILY'S OWN anti-signal bucket, so the
        # injected edge sits exactly where that family's gate claims one lives.
        sense = FAMILY_SENSE[family]
        keep, values, returns, mask = [], [], [], []
        for t in rows:
            h = (t.get("h") or {}).get(hw)
            if h is None or not math.isfinite(float(h)):
                continue
            if t.get("efficiency") is None:
                continue
            keep.append(t)
            values.append(float(t["efficiency"]))
            returns.append(float(t["pnl_pct_net"]))
            mask.append(anti_signal_side(float(h), sense))
        return keep, values, returns, mask

    specs = (
        ("1410", None, True, 30),
        ("primary", COHORT_PRIMARY, False, PRIMARY_FAMILY_SIZE),
        ("exploratory", COHORT_EXPLORATORY, False, 30),
    )
    by_pool: dict = {}
    by_pool_return: dict = {}
    for label, cohort, only_1410, family_size in specs:
        rows_all, vals_all, rets_all, mask_all, fams_all = [], [], [], [], []
        for family in FAMILIES:
            rows, vals, rets, mask = _split(_pool(family, cohort, only_1410), family)
            rows_all += rows
            vals_all += vals
            rets_all += rets
            mask_all += mask
            fams_all += [family] * len(rows)
            if label == "primary":
                fam_idx, _ = usable_cluster_rows(rows)
                fam_rows = [rows[i] for i in fam_idx]
                fam_vals = [vals[i] for i in fam_idx]
                fam_rets = [rets[i] for i in fam_idx]
                fam_mask = [mask[i] for i in fam_idx]
                out["by_family_cluster"][family] = min_detectable_effect_eff(
                    fam_rows, fam_vals, fam_mask, family_size, cluster=True,
                    n_perm=n_perm, seed=seed)
                out["by_family_cluster_return"][family] = min_detectable_effect_pp(
                    fam_rows, fam_rets, fam_mask, family_size, cluster=True,
                    n_perm=n_perm, seed=seed)

        # The cluster null defines this pool's usable sample. Apply it ONCE to
        # every number printed beside it.
        idx, _ = usable_cluster_rows(rows_all)
        rows_all = [rows_all[i] for i in idx]
        vals_all = [vals_all[i] for i in idx]
        rets_all = [rets_all[i] for i in idx]
        mask_all = [mask_all[i] for i in idx]
        fams_all = [fams_all[i] for i in idx]

        pool_obs: dict = {}
        pool_obs_ret: dict = {}
        for family in FAMILIES:
            own = [i for i, value in enumerate(fams_all) if value == family]
            sep = _separation([vals_all[i] for i in own],
                              [mask_all[i] for i in own])
            if sep is not None:
                pool_obs[f"{family}|{hw}"] = sep
            sep_ret = _separation([rets_all[i] for i in own],
                                  [mask_all[i] for i in own])
            if sep_ret is not None:
                pool_obs_ret[f"{family}|{hw}"] = sep_ret
        by_pool[label] = pool_obs
        by_pool_return[label] = pool_obs_ret
        out[f"pooled_{label}_cluster"] = min_detectable_effect_eff(
            rows_all, vals_all, mask_all, family_size, cluster=True,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_free"] = min_detectable_effect_eff(
            rows_all, vals_all, mask_all, family_size, cluster=False,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_cluster_return"] = min_detectable_effect_pp(
            rows_all, rets_all, mask_all, family_size, cluster=True,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_n"] = len(rows_all)
        if rows_all and 0 < int(np.sum(mask_all)) < len(mask_all):
            out[f"pooled_{label}_cluster_p0"] = (
                cluster_permutation_pvalue_group_diff(
                    rows_all, vals_all, mask_all, n_perm=n_perm, seed=seed)
                .get("p"))
            out[f"pooled_{label}_free_p0"] = permutation_pvalue_group_diff(
                vals_all, mask_all, n_perm=n_perm, seed=seed)
            out[f"pooled_{label}_cluster_return_p0"] = (
                cluster_permutation_pvalue_group_diff(
                    rows_all, rets_all, mask_all, n_perm=n_perm, seed=seed)
                .get("p"))
        else:
            out[f"pooled_{label}_cluster_p0"] = None
            out[f"pooled_{label}_free_p0"] = None
            out[f"pooled_{label}_cluster_return_p0"] = None

    out["observed_separation_by_pool"] = by_pool
    out["observed_separation_pp_by_pool"] = by_pool_return
    out["primary_target"] = PRIMARY_TARGET
    out["continuity_target"] = CONTINUITY_TARGET
    out["horizon_hours"] = HORIZON_HOURS
    return out


# ---------------------------------------------------------------------------
# Report rendering. `## Recommendation` is always the FINAL section and is a
# mechanical render of decide_recommendation — never hand-written.
# ---------------------------------------------------------------------------

def _fmt(value, digits: int = 2, suffix: str = "") -> str:
    if value is None:
        return "-"
    if isinstance(value, float) and not math.isfinite(value):
        return "-"
    return f"{value:.{digits}f}{suffix}"


def _fmt_p(value) -> str:
    return "-" if value is None else f"{value:.4f}"


def _resolve_winner(winner, configs: Sequence[dict]):
    """The winner as a full config dict, whether it arrived live or via JSON.

    ``decide_recommendation`` hands the live path the config itself; the
    committed payload stores only its id. Re-hydrating here is what keeps
    ``--render-only`` byte-identical with the scoring run on the one branch
    that reads a winner's own numbers.
    """
    if winner is None or isinstance(winner, dict):
        return winner
    for cfg in configs or ():
        if cfg.get("config_id") == winner:
            return cfg
    raise AssertionError(
        f"decision names winner {winner!r} but no such config is in the payload")


def render_recommendation(decision: dict, mde: dict,
                          configs: Sequence[dict] = ()) -> str:
    lines = ["## Recommendation", ""]
    gate = decision.get("validity_gate") or validity_gate(mde)
    if decision["verdict"] != VERDICT_CONFIG:
        lines.append(VERDICT_LABELS[
            VERDICT_RESOLVED_NULL if gate["passed"] else VERDICT_INCONCLUSIVE])
        lines.append("")
        lines.append(decision["justification"])
        lines.append("")
        lines.append("### The pre-registered key risk, and what happened")
        lines.append("")
        lines.append(f"> {KEY_RISK_PREDICTION}")
        lines.append("")
        if decision.get("key_risk_held"):
            lines.append(
                "The prediction HELD. This design could see an effect of the "
                "size its own buckets show, and the cluster null still refused "
                "the contrast. That is the answer the issue asked for: the "
                "question closes here as \"no usable Hurst edge at or above the "
                "measured limit\", and NOT with another request for more data. "
                "Anyone reopening it must bring a different MECHANISM — a "
                "different statistic of H, a different decision rule — and must "
                "pre-register it before reading these numbers.")
        else:
            lines.append(
                "The prediction is UNRESOLVED, because the validity gate did "
                "not pass: this design still cannot see an effect the size its "
                "own buckets show, so its null cannot distinguish \"whole "
                "datasets moving together\" from \"a small real effect\". The "
                "honest reading is the same one #1422 published, now with a "
                "tighter bound: an edge at or above the measured limit would "
                "have been caught, and none was.")
        lines.append("")
        lines.append(
            "#1411's `hurst_gate` stays DEFAULT-OFF with no recommended "
            "thresholds. Nothing in this report licenses shipping one, and "
            "`config.example.json` carries no `hurst_gate` block.")
        return "\n".join(lines) + "\n"

    lines.append(VERDICT_LABELS[VERDICT_CONFIG])
    lines.append("")
    if gate["passed"]:
        lines.append("The validity gate passed: " + _render_gate_sentence(gate)
                     + " A positive result on this design is therefore readable "
                       "as evidence about the market.")
    else:
        lines.append(
            "READ THIS FIRST — the validity gate did NOT pass: "
            + _render_gate_sentence(gate)
            + " The gate bounds what a NULL from this design could mean, so a "
              "positive result is not void; it does mean the raw anti-signal "
              "split this design can resolve is weaker than the tested rule's "
              "own contrast, which is an unusual combination and deserves "
              "scrutiny before anything is built on it.")
    lines.append("")
    for family in FAMILIES:
        entry = decision["families"][family]
        winner = _resolve_winner(entry["winner"], configs)
        lines.append(f"### {family}")
        lines.append("")
        if entry["n_tested"] == 0:
            lines.append(
                f"Not tested confirmatorily. The single pre-registered primary "
                f"hypothesis belongs to `{PRIMARY_FAMILY}`, so this family is "
                f"EXPLORATORY-ONLY in this study and can produce no "
                f"recommendation. That is Route 1's stated cost.")
            lines.append("")
            continue
        if winner is None:
            lines.append(
                f"No configuration of the {entry['n_tested']} primary "
                f"hypotheses tested beat ungated under the pre-registered rule. "
                f"Do not gate or size the {family} family on the Hurst exponent.")
            lines.append("")
            continue
        proto = {w: (winner["windows"].get(w) or {}) for w in winner["protocol_windows"]}
        evidence = "; ".join(
            f"{w}: drawdown {_fmt(proto[w].get('dd_delta'), 2, ' pp')}, "
            f"chop {_fmt(proto[w].get('chop_delta'), 2, ' pp')}, "
            f"return {_fmt(proto[w].get('ret_gated'))} vs "
            f"{_fmt(proto[w].get('ret_ungated'))}"
            for w in winner["protocol_windows"])
        if winner["mode"] == "gate":
            lines.append("- Mode: **gate** (hard entry gate, hysteresis)")
            lines.append(f"- Arm / disarm: **{winner['arm']:g} / {winner['disarm']:g}** "
                         f"({winner['sense']})")
        else:
            lines.append("- Mode: **size** (entry size multiplier, no hard gate)")
            lines.append(f"- Gain: **{winner['gain']:g}**, "
                         f"m = clamp(1 + gain x e, {SIZING_CLAMP_LO:g}, "
                         f"{SIZING_CLAMP_HI:g}), NaN -> "
                         f"{SIZING_NAN_MULTIPLIER:g} ({winner['sense']})")
        lines.append(f"- Hurst window length: **{winner['hurst_window']} bars**")
        lines.append(f"- Evidence on the primary target: cluster "
                     f"p={_fmt_p(winner['p_cluster'])} "
                     f"(free-shuffle p={_fmt_p(winner['p_raw'])}), "
                     f"Benjamini-Hochberg significant at alpha={ALPHA} against a "
                     f"denominator of {PRIMARY_FAMILY_SIZE}; net-return "
                     f"continuity cluster p={_fmt_p(winner.get('p_cluster_return'))}.")
        lines.append(f"- Volume: {_fmt(winner['n_suppressed_effective'], 1)} effective "
                     f"suppressed / {_fmt(winner['n_kept_effective'], 1)} effective "
                     f"kept trades (of {winner['n_suppressed']}/{winner['n_kept']} "
                     f"nominal).")
        lines.append(f"- Economics on NET RETURN: {evidence}.")
        lines.append(f"- Config id: `{winner['config_id']}` "
                     f"({entry['n_passing']}/{entry['n_tested']} primary configs "
                     f"passed).")
        lines.append("")
    lines.append(
        "This is a research recommendation and not a deployment. #1411's "
        "`hurst_gate` is default-off; shipping any threshold is a separate, "
        "explicit decision.")
    return "\n".join(lines).rstrip() + "\n"


def _render_gate_sentence(gate: dict) -> str:
    if gate.get("limit") is None or gate.get("largest_separation") is None:
        return gate.get("reason") or "the gate could not be evaluated."
    relation = "BELOW" if gate["passed"] else "ABOVE"
    return (f"the primary cohort's detection limit is "
            f"{gate['limit']:.3f} efficiency units, {relation} the "
            f"{gate['largest_separation']:.3f} that same cohort separates by.")


def _render_bucket_table(table: dict) -> list:
    lines = [
        "| Bucket | Trades | Win rate | Mean net % | Median net % | "
        "Mean efficiency | Eff. rows | Compounded % | Trade-seq max DD % | "
        "Chop loss |",
        "|--------|-------:|---------:|-----------:|-------------:|"
        "----------------:|----------:|-------------:|-------------------:|"
        "----------:|",
    ]
    for bucket in BUCKETS:
        row = table.get(bucket) or {}
        lines.append(
            f"| `{bucket}` | {row.get('trades', 0)} | "
            f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
            f"{_fmt(row.get('mean_pnl_pct_net'))} | "
            f"{_fmt(row.get('median_pnl_pct_net'))} | "
            f"{_fmt(row.get('mean_efficiency'), 4)} | "
            f"{row.get('efficiency_trades', 0)} | "
            f"{_fmt(row.get('compounded_return_pct'))} | "
            f"{_fmt(row.get('trade_seq_max_dd_pct'))} | "
            f"{_fmt(row.get('chop_loss_pct'))} |")
    lines.append("")
    return lines


def _render_config_table(cfgs: Sequence[dict], protocol: Sequence[str]) -> list:
    head = ("| Config | Mode | W | Pooled N (eff) | sup/kept eff | free p (eff) | "
            "cluster p (eff) | cluster p (net ret) | BH sig |")
    sep = ("|--------|------|--:|----------------|--------------|-------------:|"
           "---------------:|--------------------:|:------:|")
    for w in protocol:
        head += f" {w} dd | {w} chop | {w} ret (arm/base) |"
        sep += "------:|--------:|-------------------|"
    head += " Verdict |"
    sep += "---------|"
    lines = [head, sep]
    for cfg in cfgs:
        row = (f"| `{cfg['config_id']}` | {cfg['mode']} | {cfg['hurst_window']} | "
               f"{cfg['n_pooled_trades']} ({_fmt(cfg['n_pooled_effective'], 1)}) | "
               f"{_fmt(cfg['n_suppressed_effective'], 1)}/"
               f"{_fmt(cfg['n_kept_effective'], 1)} | "
               f"{_fmt_p(cfg['p_raw'])} | {_fmt_p(cfg['p_cluster'])} | "
               f"{_fmt_p(cfg.get('p_cluster_return'))} | "
               f"{'yes' if cfg.get('bh_reject') else 'no'} |")
        for w in protocol:
            r = cfg["windows"].get(w) or {}
            if not r.get("n_legs"):
                row += " - | - | - |"
            else:
                row += (f" {_fmt(r['dd_delta'])} | {_fmt(r['chop_delta'])} | "
                        f"{_fmt(r['ret_gated'])} / {_fmt(r['ret_ungated'])} |")
        ok, reasons = config_verdict(cfg)
        row += f" {'PASS' if ok else '; '.join(reasons)} |"
        lines.append(row)
    lines.append("")
    return lines


def _render_joint_table(table: dict) -> list:
    lines = [
        "| ADX | H | Trades | Win rate | Mean net % | Compounded % | Trade-seq max DD % |",
        "|-----|---|-------:|---------:|-----------:|-------------:|-------------------:|",
    ]
    for a in JOINT_ADX_BUCKETS:
        for h in JOINT_H_BUCKETS:
            row = table.get(f"{a}|{h}") or {}
            lines.append(
                f"| `{a}` | `{h}` | {row.get('trades', 0)} | "
                f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
                f"{_fmt(row.get('mean_pnl_pct_net'))} | "
                f"{_fmt(row.get('compounded_return_pct'))} | "
                f"{_fmt(row.get('trade_seq_max_dd_pct'))} |")
    lines.append("")
    return lines


def render_report(payload: dict) -> str:
    """Full Markdown report. `## Recommendation` is guaranteed to be last."""
    pre = payload["pre_registered"]
    run = payload["run_summary"]
    cfgs = payload["configs"]
    mde = payload.get("mde") or {}
    decision = payload["decision"]
    hurst_windows = pre["hurst_windows"]
    gate = decision.get("validity_gate") or validity_gate(mde)

    out = []
    out.append("# Hurst gate resolution study (#1424)")
    out.append("")
    out.append(
        "Report-only evidence on whether a Hurst-based entry gate is worth "
        "building. Nothing here is wired to the scheduler, to config, or to any "
        "live path. This file is the LIVE-EVIDENCE CONTRACT PATH: "
        "`scheduler/hurst_gate.go`, `docs/ARCHITECTURE.md` and #1412's Stage 0 "
        "gate all read it. It supersedes the #1422 power study, whose own "
        "render now lives at `hurst_1422_gate_power.md`; #1410's is at "
        "`hurst_1410_gate_calibration.md`.")
    out.append("")
    out.append(
        "#1422 returned INCONCLUSIVE because of POWER: its primary cohort could "
        "resolve 0.96 pp per trade and that cohort separated by only 0.37 pp, "
        "so the null bounded the edge from above and said nothing about "
        "anything smaller. This study exists to make its OWN null readable. It "
        f"lowers the detection limit three ways at once — ONE pre-registered "
        f"hypothesis instead of four, {len(PRE2020_WINDOWS)} added pre-2020 "
        f"calendar windows from two venues the `binanceus` cache cannot reach "
        f"(the coverage table below says which of them survived), and a "
        "bounded-variance primary target — and it publishes a mechanical "
        "VALIDITY GATE that decides, before any interpretation, whether the "
        "result is a statement about the market or about the design.")
    out.append("")
    out.append(
        f"Generated by `backtest/research/hurst_1424_gate_resolution.py` (schema "
        f"{payload['schema_version']}). Every number below is rendered from "
        f"`hurst_1424_gate_resolution.json`, produced by the same run.")
    out.append("")

    out.append("## Verdict at a glance")
    out.append("")
    out.append(f"- Validity gate: **{'PASSED' if gate['passed'] else 'FAILED'}** — "
               + _render_gate_sentence(gate))
    out.append(f"- Recommendation: "
               f"**{VERDICT_LABELS.get(decision['verdict'], decision['verdict'].upper())}**.")
    out.append(f"- Pre-registered key risk: "
               f"**{'HELD' if decision.get('key_risk_held') else 'UNRESOLVED'}**.")
    out.append("")

    out.append("## Pre-registered design")
    out.append("")
    out.append(
        "These constants live at the top of the study script and were fixed "
        "before the sweep ran. The Recommendation is the mechanical output of "
        "the acceptance rule applied to them.")
    out.append("")
    out.append(
        "- Estimator: `hurst_exponent` from "
        "`shared_strategies/open/indicators_core.py` (#1409 SSoT). Never "
        "reimplemented here.")
    out.append(
        "- NaN policy: NaN is its OWN bucket for BOTH H and ADX, never coerced "
        "to 0.5 / 25. It neither arms nor disarms the gate (state holds) and "
        "gives a size multiplier of exactly 1.")
    out.append(f"- Hurst window lengths: {', '.join(str(h) for h in hurst_windows)} bars.")
    out.append(f"- Buckets on H at entry: {', '.join('`' + b + '`' for b in BUCKETS)}.")
    out.append(
        f"- PRIMARY TARGET (Route 3): `{PRIMARY_TARGET}` over a fixed "
        f"{HORIZON_HOURS}-hour horizon. For a fill at bar `i` in direction `d` "
        f"over `K` bars, `d * (close[i+K] - close[i])` divided by the summed "
        f"absolute bar-to-bar path, so the value is bounded in `[-1, 1]`. The "
        f"horizon is fixed in CALENDAR time, so 1h and 4h datasets measure the "
        f"same four days and pool directly. It is an OUTCOME, exactly as PnL "
        f"is, so no look-ahead invariant moves. A trade with fewer than `K` "
        f"bars left in its window slice is EXCLUDED and counted, never "
        f"truncated to a shorter horizon.")
    out.append(
        f"- CONTINUITY TARGET: `{CONTINUITY_TARGET}`, scored on the SAME rows "
        f"so the two describe one pool. The verdict's significance reads the "
        f"PRIMARY target; the ECONOMICS read net return, because a "
        f"bounded-variance win is evidence about the mechanism and never on its "
        f"own a licence to ship a gate.")
    out.append(
        f"- PRIMARY HYPOTHESIS (Route 1): exactly one — `{PRIMARY_CONFIG_ID}` — "
        f"derived mechanically as the smallest raw p over ALL 30 configs in the "
        f"committed `hurst_1410_gate_calibration.json`, pinned here, and "
        f"re-derived at run time with a hard assert. The Benjamini-Hochberg "
        f"denominator for the confirmatory family is therefore "
        f"{PRIMARY_FAMILY_SIZE}, and its rank-1 bar is alpha rather than "
        f"alpha/4. THE COST IS REAL: this study tests ONE gate design and "
        f"cannot rank alternatives. `mean_reversion` is exploratory-only here.")
    out.append("- Windows: " + "; ".join(
        f"`{k}` {v[0]}..{v[1] or 'latest'}" for k, v in pre["windows"].items()) + ".")
    out.append(f"- Datasets ({len(pre['datasets'])}): "
               f"{', '.join('`' + d + '`' for d in pre['datasets'])}.")
    out.append(f"- Fee model: `{pre['fee_platform']}` (plus the Backtester's 5 bps "
               f"slippage default) on every venue. The DATA-SOURCE exchange and "
               f"the FEE model are independent axes and are never coupled — the "
               f"early tape is priced at today's fee model on purpose, because "
               f"this study measures whether H SORTS trades and not what those "
               f"trades would historically have earned.")
    out.append(
        f"- Primary cohort economics: protocol "
        f"{', '.join('`' + w + '`' for w in PRIMARY_PROTOCOL_WINDOWS)} (two bull "
        f"years, two bear years), of which at least "
        f"{PRIMARY_PROTOCOL_MIN_WINDOWS} carrying legs must hold; held-out is "
        f"every other window under the unchanged 2/3 rule.")
    out.append(
        f"- Exploratory cohort economics: #1410's split verbatim — protocol "
        f"{', '.join('`' + w + '`' for w in EXPLORATORY_PROTOCOL_WINDOWS)}; held-out "
        f"{', '.join('`' + w + '`' for w in EXPLORATORY_HELD_OUT_WINDOWS)}.")
    out.append(
        f"- Volume floors on EFFECTIVE N: >= {MIN_SUPPRESSED_EFFECTIVE:g} suppressed "
        f"and >= {MIN_KEPT_EFFECTIVE:g} kept.")
    out.append(
        f"- Inference: free-shuffle permutation (for continuity) AND the shared "
        f"circular calendar rotation inherited from #1422 (the cluster null, "
        f"which the verdict reads). {pre['n_perm']} draws, seed {pre['seed']}, "
        f"minimum offset {MIN_OFFSET_DAYS} days. Benjamini-Hochberg at "
        f"alpha={ALPHA}, applied SEPARATELY to the primary and exploratory "
        f"cohorts.")
    out.append(
        f"- Detection limit: the #1422 estimator with the grid in the target's "
        f"own units — {MDE_EFF_GRID_STEP:g} to {MDE_EFF_GRID_MAX:g} efficiency "
        f"units then a {MDE_EFF_REFINE_STEP:g} refinement for the primary "
        f"target, and #1422's {MDE_PP_GRID_STEP:g}-to-{MDE_PP_GRID_MAX:g} pp "
        f"grid verbatim for the continuity target so the two studies' limits "
        f"stay comparable. {pre['n_perm_mde']} draws. It RAISES rather than "
        f"returning None when the permutation count cannot resolve the rank-1 "
        f"bar.")
    out.append(f"- Joint table: ADX period {ADX_PERIOD} (Wilder), split at "
               f"{ADX_SPLIT:g}, scored on NET RETURN so its verdict stays "
               f"comparable with the one #1422 already recorded against #1412.")
    out.append("")

    out.append("### The validity gate")
    out.append("")
    out.append(
        "This study is VALID only when the measured cluster-null detection "
        "limit on its primary cohort falls BELOW that same cohort's "
        "pool-matched observed separation, both in efficiency units and both "
        "measured on the same rows. When it does, a null result is a statement "
        "about the market: no usable edge at or above the limit. When it does "
        "not, the null is a statement about the design and nothing more, and "
        "the report says exactly that rather than dressing a power shortfall as "
        "a finding.")
    out.append("")
    out.append(f"**Outcome: {'PASSED' if gate['passed'] else 'FAILED'}** — "
               + _render_gate_sentence(gate))
    out.append("")

    out.append("### Interim look, disclosed")
    out.append("")
    out.append(INTERIM_LOOK_DISCLOSURE)
    out.append("")

    out.append("### Cohorts")
    out.append("")
    out.append(
        "A cell is PRIMARY when its tape was not read by the evidence that "
        "selected the primary hypothesis — the committed #1410 JSON, and "
        "nothing else. Every new-venue cell qualifies (#1410 saw no Bitstamp or "
        "Coinbase bar, and every such window is pre-2020H2, asserted at "
        "import). A `binanceus` cell qualifies when its window or its symbol is "
        "one #1410 never scored; a new TIMEFRAME on an audit symbol over a "
        "#1410 window is the same tape resampled and stays exploratory. The "
        "#1422 primary cells are therefore primary here too — that is Route 1's "
        "dividend, and the interim look above is its price.")
    out.append("")
    out.append(
        f"The exploratory grid re-runs all {run['n_exploratory_configs']} #1410 "
        "configurations over the expanded data under its own correction. It can "
        "never produce a recommendation.")
    out.append("")

    out.append("### Look-ahead invariant")
    out.append("")
    out.append(
        "Rolling H at bar `i` uses closes `[i-W+1, i]`. The DECISION series is "
        "that shifted one bar, so a signal at bar N reads H through N-1; the "
        "backtester fills a bar-N signal at N+1's open, so the FILL-BAR stamp is "
        "the bar-close series shifted twice. Entry ADX uses the SAME two shifts. "
        "ADX warm-up bars are masked to NaN — `compute_regime` fills them with "
        "0.0, which would otherwise file every warm-up bar under \"low ADX\". "
        "The primary target reads bars AFTER the fill, which is what an outcome "
        "does; it enters no decision anywhere.")
    out.append("")

    out.append("## Route 2 — data feasibility, recorded")
    out.append("")
    out.append(
        "Route 2 asked for more INDEPENDENT calendar clusters and required a "
        "feasibility check first. These probes ran read-only against every "
        "candidate source before the design was frozen. The routes that failed "
        "are listed with their evidence rather than quietly omitted.")
    out.append("")
    out.append("| Source | Verdict | Evidence |")
    out.append("|--------|---------|----------|")
    for probe in pre.get("feasibility_probes") or ():
        out.append(f"| `{probe['source']}` | {probe['verdict']} | {probe['detail']} |")
    out.append("")
    out.append(
        "Window OWNERSHIP is what keeps the added tape from double-counting a "
        "calendar span: exactly one venue owns each (base asset, window) cell, "
        "asserted at import. Bitstamp owns BTC for "
        + ", ".join(f"`{w}`" for w in BITSTAMP_WINDOWS)
        + "; Coinbase Exchange owns BTC, ETH and LTC for "
        + ", ".join(f"`{w}`" for w in COINBASE_WINDOWS)
        + "; Binance.US owns everything from `2020H2` onward. Two symbols "
          "sharing a BASE ASSET are credited correlation 1.0 whatever their "
          "quote currency or venue, so `BTC/USD` and `BTC/USDT` can never be "
          "counted as two independent markets.")
    out.append("")
    out.append(
        "The honest limit of this route: the added CALENDAR CLUSTERS are what "
        "raise effective N. The extra Coinbase symbols inside those same years "
        "add rows and comparatively little independent information, which is "
        "why effective N is printed beside nominal N everywhere below and why "
        "the volume floors are applied to it.")
    out.append("")

    out.append("## Coverage and effective sample size")
    out.append("")
    cov = run.get("coverage") or {}
    out.append(
        f"{cov.get('n_kept', 0)} of {cov.get('n_cells', 0)} OWNED (dataset, "
        f"window) cells carried enough history to score: "
        f"{cov.get('required_lead_bars', 0)} bars of lead before the window "
        f"start, and at least "
        f"{float(cov.get('min_window_bar_fraction') or 0) * 100:.0f}% of the bars "
        f"a complete cache would hold inside the window. That density floor is "
        f"what catches a late listing or a delisting gap — a year present as a "
        f"few hundred bars is not a year. An open-ended window is closed at ONE "
        f"run-level reference bar (`{cov.get('reference_last_bar') or '-'}`), "
        f"never at each dataset's own last bar. {cov.get('n_dropped', 0)} owned "
        f"cells were DROPPED, listed below; a further "
        f"{cov.get('n_unowned', 0)} (dataset, window) pairs were never in the "
        f"design because another venue owns that calendar span.")
    out.append("")
    dropped = cov.get("dropped") or []
    if dropped:
        out.append("| Dataset | Window | Why dropped |")
        out.append("|---------|--------|-------------|")
        for d in dropped:
            out.append(f"| `{d['dataset']}` | `{d['window']}` | {d['reason']} |")
        out.append("")
    else:
        out.append("No owned cells were dropped.")
        out.append("")

    out.append(
        "Effective N is `N^2 / sum_ij rho_ij`, with `rho_ij` the symbol-level "
        "daily-return correlation when two trades' holding periods OVERLAP and 0 "
        "when they do not; same-asset pairs take 1.0, and correlations are "
        "clipped to `[0, 1]` so anti-correlation can never manufacture power. "
        "It is printed beside nominal N in every table that feeds a p-value, and "
        "the volume floors are applied to it.")
    out.append("")
    ex_names = sorted({d for c in cfgs
                       for d in (c.get("cluster_excluded_datasets") or [])})
    ex_rows = max((int(c.get("cluster_excluded_trades") or 0) for c in cfgs),
                  default=0)
    if ex_names:
        out.append(
            f"Datasets too short to host a calendar rotation, and therefore "
            f"dropped from the contrast, the counts and the effective-N floors "
            f"alike (up to {ex_rows} rows on a single config): "
            + ", ".join(f"`{d}`" for d in ex_names) + ".")
    else:
        out.append(
            "Every dataset spans enough calendar time to host a rotation, so no "
            "rows were dropped from any cluster contrast.")
    out.append("")
    excluded_target = max((int(c.get("n_missing_target") or 0) for c in cfgs),
                          default=0)
    out.append(
        f"Horizon truncation: up to {excluded_target} rows on a single config "
        f"had fewer than the horizon's bars left in their window slice and were "
        f"excluded from BOTH targets, so the primary and continuity columns "
        f"always describe one pool.")
    out.append("")

    rho = run.get("symbol_correlations") or {}
    if rho:
        syms = sorted({s for pair in rho for s in pair.split("|")})
        out.append("Symbol daily-return correlation matrix:")
        out.append("")
        out.append("| | " + " | ".join(f"`{s}`" for s in syms) + " |")
        out.append("|---|" + "---|" * len(syms))
        for a in syms:
            cells = []
            for b in syms:
                if a == b:
                    cells.append("1.00")
                else:
                    v = rho.get(f"{a}|{b}", rho.get(f"{b}|{a}"))
                    cells.append(_fmt(v))
            out.append(f"| `{a}` | " + " | ".join(cells) + " |")
        out.append("")

    out.append("## Measured detection limit")
    out.append("")
    out.append(
        "The smallest per-trade effect each pool could have detected, under the "
        "cluster null at the rank-1 Benjamini-Hochberg threshold, in the PRIMARY "
        "TARGET's units. The separation column is that same contrast measured on "
        "the SAME rows, so the two are comparable; a whole-study separation read "
        "against a sub-cohort's limit would not be. `Resolvable? = NO` means the "
        "design cannot see an effect that small — such a pool's null bounds the "
        "edge from above and says nothing about whether a smaller one exists.")
    out.append("")
    by_pool = mde.get("observed_separation_by_pool") or {}
    out.append("| Pool | Rows | BH denominator | Cluster MDE (eff) | Free MDE (eff) | "
               "Largest separation ON THAT POOL (eff) | Resolvable? | "
               "Cluster p at zero injection |")
    out.append("|------|-----:|---------------:|------------------:|---------------:|"
               "-------------------------------------:|:-----------:|"
               "----------------------------:|")
    for key, label, denom in (
            ("1410", "#1410 design (its 30-hypothesis grid)", 30),
            ("primary", "this study, primary cohort", PRIMARY_FAMILY_SIZE),
            ("exploratory", "this study, exploratory grid", 30)):
        c = mde.get(f"pooled_{key}_cluster")
        f = mde.get(f"pooled_{key}_free")
        seps = by_pool.get(key) or {}
        largest = (max(abs(float(v)) for v in seps.values()) if seps else None)
        if largest is None or c is None:
            resolvable = "-"
        else:
            resolvable = "yes" if largest >= c else "NO"
        out.append(f"| {label} | {mde.get(f'pooled_{key}_n', 0)} | {denom} | "
                   f"{'> ' + f'{MDE_EFF_GRID_MAX:g}' if c is None else _fmt(c, 3)} | "
                   f"{'> ' + f'{MDE_EFF_GRID_MAX:g}' if f is None else _fmt(f, 3)} | "
                   f"{_fmt(largest, 3)} | {resolvable} | "
                   f"{_fmt_p(mde.get(f'pooled_{key}_cluster_p0'))} |")
    out.append("")
    out.append(
        "The same three pools on the CONTINUITY target (percentage points of "
        "net return), on #1422's grid so the two studies are directly "
        "comparable:")
    out.append("")
    by_pool_pp = mde.get("observed_separation_pp_by_pool") or {}
    out.append("| Pool | Cluster MDE (pp/trade) | Largest separation ON THAT POOL "
               "(pp/trade) | Resolvable? | Cluster p at zero injection |")
    out.append("|------|-----------------------:|"
               "------------------------------------------:|:-----------:|"
               "----------------------------:|")
    for key, label in (("1410", "#1410 design (its 30-hypothesis grid)"),
                       ("primary", "this study, primary cohort"),
                       ("exploratory", "this study, exploratory grid")):
        c = mde.get(f"pooled_{key}_cluster_return")
        seps = by_pool_pp.get(key) or {}
        largest = (max(abs(float(v)) for v in seps.values()) if seps else None)
        if largest is None or c is None:
            resolvable = "-"
        else:
            resolvable = "yes" if largest >= c else "NO"
        out.append(f"| {label} | "
                   f"{'> ' + f'{MDE_PP_GRID_MAX:g}' if c is None else _fmt(c)} | "
                   f"{_fmt(largest)} | {resolvable} | "
                   f"{_fmt_p(mde.get(f'pooled_{key}_cluster_return_p0'))} |")
    out.append("")

    out.append("## Part A - outcomes bucketed by H at entry")
    out.append("")
    out.append(
        "Ungated legs only, pooled per family across datasets and windows and "
        "deduplicated on `(strategy, symbol, timeframe, entry_date)` with the "
        "symbol venue-qualified. Drawdown here is TRADE-GRANULAR (the compounded "
        "trade sequence), not the bar-level engine drawdown used in Part B. "
        "`Mean efficiency` is the PRIMARY TARGET — the column the validity gate "
        "adjudicates.")
    out.append("")
    out.append(study1410.render_nan_bucket_note(run.get("warmup")))
    out.append("")
    for family in FAMILIES:
        out.append(f"### {family}")
        out.append("")
        for hw in hurst_windows:
            out.append(f"**Hurst window {hw} bars**")
            out.append("")
            out.extend(_render_bucket_table(
                (payload["buckets"].get(family) or {}).get(str(hw)) or {}))

    out.append("## Part B / C - the primary hypothesis")
    out.append("")
    out.append(
        "`gate` rows are real Backtester re-runs with entry signals masked while "
        "the gate is disarmed (closes never masked); their drawdowns are "
        "bar-level. `size` rows re-compound the same ungated trade sequence with "
        "the size multiplier; their drawdowns are trade-granular. Never compare a "
        "`gate` drawdown to a `size` drawdown. `dd` and `chop` are MAGNITUDE "
        "deltas (arm minus ungated) averaged over that window's legs — negative "
        "means improvement. Significance reads the CLUSTER p on the PRIMARY "
        "TARGET; the economics columns are NET RETURN.")
    out.append("")
    primary = [c for c in cfgs if c["cohort"] == COHORT_PRIMARY]
    out.extend(_render_config_table(primary, PRIMARY_PROTOCOL_WINDOWS))

    out.append("## Part B / C - exploratory grid (#1410's configs, expanded data)")
    out.append("")
    out.append(
        "Reported for completeness under its OWN Benjamini-Hochberg correction. "
        "These hypotheses were selected and scored on overlapping evidence, so "
        "nothing here can produce a recommendation.")
    out.append("")
    exploratory = [c for c in cfgs if c["cohort"] == COHORT_EXPLORATORY]
    out.extend(_render_config_table(exploratory, EXPLORATORY_PROTOCOL_WINDOWS))

    out.append("## Part D - joint ADX x Hurst buckets (#1412 Stage 0)")
    out.append("")
    out.append(
        "The question #1412's Stage 0 asks: do joint buckets separate beyond "
        "what either metric alone predicts — specifically, is high-ADX + "
        "low-Hurst materially worse for momentum-style entries than high-ADX "
        "alone? Material separation requires BOTH a Bonferroni-corrected "
        "significant cluster p AND an effect at least as large as the detection "
        "limit measured on THIS contrast's own rows. Scored on NET RETURN, "
        "because Stage 0 asks an economic question and #1422 discharged it on "
        "that statistic; changing the statistic would produce a verdict that "
        "cannot be compared with the recorded one.")
    out.append("")
    joint = payload.get("joint") or {}
    for family in FAMILIES:
        entry = joint.get(family) or {}
        out.append(f"### {family}")
        out.append("")
        if not entry:
            out.append("No trades pooled for this family.")
            out.append("")
            continue
        out.extend(_render_joint_table(entry.get("table") or {}))
        v = entry.get("verdict") or {}
        if v.get("separated"):
            out.append(
                f"**Separation found.** Within `ADX >= {ADX_SPLIT:g}`, "
                f"`H < 0.45` entries underperform `H >= 0.45` entries by "
                f"{_fmt(v.get('delta_mean_pp'))} pp per trade "
                f"(cluster p={_fmt_p(v.get('p_cluster'))}, Bonferroni bar "
                f"{JOINT_ALPHA:g}; detection limit "
                f"{_fmt(v.get('mde_pp'))} pp).")
        else:
            out.append(f"**{NO_JOINT_SEPARATION}** — {v.get('reason') or 'no contrast'}.")
        out.append("")

    all_sep = [(joint.get(f) or {}).get("verdict", {}).get("separated")
               for f in FAMILIES]
    if any(all_sep):
        out.append(
            "At least one family separates on the joint buckets, so #1412's "
            "Stage 0 gate is satisfied for that family and Stage 1 may proceed "
            "on this evidence.")
    else:
        out.append(
            f"`{NO_JOINT_SEPARATION}` on every family, on a pool larger than the "
            f"one #1422 discharged Stage 0 against. #1412 stays closed: full "
            f"fusion of Hurst into the composite classifier is not justified, "
            f"and the standalone `hurst_gate` (#1411), which ships default-off, "
            f"remains the correct amount of Hurst.")
    out.append("")

    out.append("## Acceptance rule")
    out.append("")
    out.append("The primary config wins for its family only when ALL of the following hold:")
    out.append("")
    out.append(f"1. Effective suppressed trades >= {MIN_SUPPRESSED_EFFECTIVE:g} and "
               f"effective kept trades >= {MIN_KEPT_EFFECTIVE:g}.")
    out.append(f"2. Significant after Benjamini-Hochberg at alpha={ALPHA} ON THE "
               f"CLUSTER p OF THE PRIMARY TARGET, against a denominator of "
               f"{PRIMARY_FAMILY_SIZE}.")
    out.append(f"3. On at least {PRIMARY_PROTOCOL_MIN_WINDOWS} of the "
               f"{len(PRIMARY_PROTOCOL_WINDOWS)} protocol windows that carry "
               f"legs: mean drawdown magnitude falls, chop loss falls, and the "
               f"NET-RETURN give-up stays within "
               f"max({RETURN_TOLERANCE_PP:g} pp, "
               f"{RETURN_TOLERANCE_FRAC:g} x |ungated return|).")
    out.append("4. Drawdown does not degrade on at least 2/3 of the held-out windows "
               f"that carry legs, with at least {HELD_OUT_MIN_WINDOWS} such windows.")
    out.append("")
    out.append(
        "Rules 1 and 2 are what separate a real edge from arithmetic. ANY gate "
        "that removes trades lowers drawdown and chop loss on a losing book, so "
        "those columns alone prove nothing. Rule 3 is the reason a win on the "
        "bounded-variance target cannot ship on its own: the mechanism question "
        "and the money question are asked separately and both must answer yes.")
    out.append("")

    out.append("## Run summary")
    out.append("")
    out.append(f"- Legs scored: {run['legs']} ungated + {run['gated_arms']} gated arms.")
    out.append(f"- Harness identity: {run['mirror_verified_legs']} of {run['legs']} "
               f"ungated legs reproduced `eval_windows.run_leg` exactly, including "
               f"every new-venue leg.")
    out.append("- Pooled deduplicated trades: " + "; ".join(
        f"{f} {run['pooled_trades'][f]} "
        f"(primary {run['pooled_primary'][f]}, exploratory "
        f"{run['pooled_exploratory'][f]})" for f in FAMILIES) + ".")
    out.append(f"- Hypotheses: {run['n_primary_configs']} primary, "
               f"{run['n_exploratory_configs']} exploratory; "
               f"Benjamini-Hochberg-significant: {run['n_primary_significant']} "
               f"primary, {run['n_exploratory_significant']} exploratory.")
    warm = run.get("warmup") or {}
    out.append(f"- Warm-up lead before each dataset's own earliest scored window: min "
               f"{warm.get('min_lead_bars', 0)} bars, required "
               f"{warm.get('required_bars', 0)} — "
               f"{'sufficient on every dataset' if warm.get('sufficient') else 'SHORT on ' + ', '.join(warm.get('insufficient_datasets') or [])}.")
    out.append(f"- Wall time: {run['elapsed_sec']} s.")
    out.append("")

    out.append(render_recommendation(decision, mde, cfgs))
    return "\n".join(out).rstrip() + "\n"


def report_from_payload(payload: dict) -> str:
    """Render straight from a committed JSON (the ``--render-only`` path)."""
    return render_report(payload)


# ---------------------------------------------------------------------------
# Data acquisition.
# ---------------------------------------------------------------------------

def ensure_min_history(datasets: Sequence[tuple]) -> dict:
    """Backfill each dataset to its venue's history floor.

    ``load_cached_data`` fetches ONLY when the cached slice comes back empty,
    so a 2013 request against a 2020-start cache silently returns 2020+ rows and
    never backfills. This calls ``fetch_full_history(..., store=True)``
    explicitly with that venue's measured page cap; ``store_ohlcv`` upserts, so
    re-running is a no-op. A venue that never listed a pair fails here and the
    coverage audit drops its cells — one missing symbol never aborts the study.
    """
    from data_fetcher import fetch_full_history
    report = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        since = history_since_for(dataset)
        limit = FETCH_PAGE_LIMIT.get(exchange_id, 500)
        try:
            df = fetch_full_history(symbol, timeframe, since=since,
                                    exchange_id=exchange_id, store=True,
                                    limit=limit)
            if df.empty:
                # An empty backfill is never "fine": it means the floor sits
                # before the pair's listing and the fetch stopped on the first
                # empty page. Say so loudly here — the coverage audit would
                # otherwise report it as an ordinary data gap much later.
                print(f"[1424] backfill returned NO BARS for {key} since "
                      f"{since}; the history floor is probably before this "
                      f"pair's listing date", flush=True)
            report[key] = {"ok": bool(len(df)), "bars": int(len(df)),
                           "since": since}
        except Exception as exc:  # pragma: no cover - network dependent
            report[key] = {"ok": False, "since": since,
                           "error": f"{type(exc).__name__}: {exc}"}
            print(f"[1424] backfill FAILED for {key}: {exc}", flush=True)
            traceback.print_exc()
    return report


def _parse_datasets(raw: Optional[str]) -> list:
    if not raw:
        return list(DATASETS)
    from eval_windows import parse_dataset_arg
    out = []
    for token in raw.split(","):
        token = token.strip()
        if not token:
            continue
        exchange_id, sep, rest = token.partition("=")
        if not sep:
            exchange_id, rest = PLATFORM, token
        symbol, timeframe = parse_dataset_arg(rest)
        out.append((exchange_id.strip(), symbol, timeframe))
    return out


def _parse_windows(raw: Optional[str]) -> list:
    if not raw:
        return list(WINDOW_ORDER)
    names = [t.strip() for t in raw.split(",") if t.strip()]
    for name in names:
        if name not in WINDOWS:
            raise SystemExit(f"unknown window {name!r}; known: {sorted(WINDOWS)}")
    return [w for w in WINDOW_ORDER if w in names]


def resolve_primary_config_id(json_path: str) -> str:
    """argmin(p_raw) over ALL configs in the committed #1410 JSON.

    Returned for the assertion against ``PRIMARY_CONFIG_ID``: the pinned value
    is what the pre-registration promises, and this is what the data actually
    says. A regenerated #1410 JSON that moves the argmin fails loud instead of
    silently swapping the single confirmatory hypothesis.

    Note what this DOES NOT read: #1422's outcomes. The pick must come from
    #1410's evidence alone, because the primary cohort here includes cells
    #1422 scored.
    """
    with open(json_path) as fh:
        payload = json.load(fh)
    best = None
    for cfg in payload.get("configs") or []:
        if cfg.get("p_raw") is None:
            continue
        cand = (float(cfg["p_raw"]), str(cfg["config_id"]))
        if best is None or cand < best:
            best = cand
    if best is None:
        raise SystemExit(f"{json_path} carries no config with a raw p-value")
    return best[1]


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--jobs", type=int, default=4, help="worker threads")
    p.add_argument("--out-dir", default=None,
                   help="optional dir for the rolling-Hurst npz cache")
    p.add_argument("--only", default=None,
                   help=f"comma-separated families to run ({', '.join(FAMILIES)})")
    p.add_argument("--windows", default=None, help="comma-separated window names")
    p.add_argument("--datasets", default=None,
                   help="comma-separated [EXCHANGE=]SYMBOL:TIMEFRAME")
    p.add_argument("--hurst-windows", default=None,
                   help="comma-separated rolling Hurst window lengths")
    p.add_argument("--n-perm", type=int, default=N_PERM)
    p.add_argument("--n-perm-mde", type=int, default=N_PERM_MDE)
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json-out", default=_DEFAULT_JSON_OUT)
    p.add_argument("--report-out", default=_DEFAULT_REPORT_OUT)
    p.add_argument("--write-report", action="store_true",
                   help="render the Markdown report at the contract path")
    p.add_argument("--no-mirror-check", action="store_true",
                   help="skip the per-leg eval_windows.run_leg identity check")
    p.add_argument("--skip-fetch", action="store_true",
                   help="run on the cache as-is; the coverage audit decides "
                        "which cells exist")
    p.add_argument("--fetch-only", action="store_true",
                   help="backfill history and exit")
    p.add_argument("--render-only", action="store_true",
                   help="re-render the report from an existing --json-out; "
                        "runs no backtests")
    args = p.parse_args(argv)

    # The committed JSON and the contract report describe the PRE-REGISTERED
    # design. A scoped debug run produces a different study, so it may not
    # occupy either path.
    scope = {
        "only": args.only,
        "datasets": args.datasets,
        "windows": args.windows,
        "hurst_windows": args.hurst_windows,
    }
    scope["complete"] = not any(v for v in scope.values())
    # --fetch-only writes neither artifact, so scoping it to the venues that
    # actually need a backfill must not be refused.
    if not scope["complete"] and not args.fetch_only:
        narrowed = ", ".join(f"--{k.replace('_', '-')} {v}"
                             for k, v in scope.items() if k != "complete" and v)
        if os.path.abspath(args.json_out) == os.path.abspath(_DEFAULT_JSON_OUT):
            raise SystemExit(
                f"[1424] refusing to overwrite the committed aggregate "
                f"{_DEFAULT_JSON_OUT} from a scoped run ({narrowed}). Pass an "
                f"explicit --json-out.")
        if os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT):
            raise SystemExit(
                f"[1424] refusing to target the live-evidence contract path "
                f"{_DEFAULT_REPORT_OUT} from a scoped run ({narrowed}). Pass an "
                f"explicit --report-out.")

    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        is_contract = (os.path.abspath(args.report_out)
                       == os.path.abspath(_DEFAULT_REPORT_OUT))
        if is_contract:
            # Fail closed: a payload with no scope stamp cannot prove it came
            # from a complete run.
            stamped = ((payload.get("run_summary") or {}).get("scope")
                       or {}).get("complete")
            if not stamped:
                raise SystemExit(
                    f"[1424] {args.json_out} is not stamped as a complete run, "
                    f"so it may not be rendered to the contract path "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not args.write_report:
                raise SystemExit(
                    "[1424] writing the contract report needs --write-report, "
                    "on --render-only exactly as on a scoring run.")
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1424] re-rendered {args.report_out} from {args.json_out}")
        return 0

    datasets = _parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print("[1424] backfill complete")
        return 0

    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(",") if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(f"unknown family {f!r}; known: {list(FAMILIES)}")
        families = tuple(f for f in FAMILIES if f in wanted)
    window_names = _parse_windows(args.windows)
    hurst_windows = (tuple(int(t) for t in args.hurst_windows.split(","))
                     if args.hurst_windows else HURST_WINDOWS)

    # The single primary hypothesis is pre-registered; verify the committed
    # #1410 evidence still yields exactly it before anything is scored.
    resolved = resolve_primary_config_id(_JSON_1410)
    if resolved != PRIMARY_CONFIG_ID:
        raise SystemExit(
            f"pre-registered primary hypothesis {PRIMARY_CONFIG_ID!r} no longer "
            f"matches the committed #1410 argmin {resolved!r}. Re-register "
            f"deliberately; never let it drift.")

    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f"[1424] backfilling {len(datasets)} datasets...")
        backfill = ensure_min_history(datasets)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    print(f"[1424] loading {len(datasets)} datasets from the venue caches...")
    frames = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        try:
            frames[dataset] = load_cached_data(symbol, timeframe,
                                               exchange_id=exchange_id)
        except Exception as exc:  # pragma: no cover - network dependent
            print(f"[1424] load FAILED for {exchange_id} "
                  f"{dataset_key(symbol, timeframe)}: {exc}")
            frames[dataset] = pd.DataFrame()

    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[1424] coverage: {coverage['n_kept']}/{coverage['n_cells']} owned cells "
          f"kept, {coverage['n_dropped']} dropped, {coverage['n_unowned']} not owned")
    for d in coverage["dropped"]:
        print(f"[1424]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    def _cell_ok(dataset, window):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        return bool(coverage["cells"].get(f"{key}|{window}"))

    usable_datasets = [ds for ds in datasets
                       if any(_cell_ok(ds, w) for w in window_names)]
    if not usable_datasets:
        raise SystemExit("[1424] no dataset carries a scoreable cell; nothing to do")

    scored_windows = [w for w in window_names
                      if any(_cell_ok(ds, w) for ds in usable_datasets)]
    first_needed_by_ds = {}
    for ds in usable_datasets:
        own = [w for w in scored_windows if _cell_ok(ds, w)]
        first_needed_by_ds[ds] = min(pd.Timestamp(WINDOWS[w][0]) for w in own)

    warmup = warmup_audit(
        scored_warmup_leads(frames, coverage, scored_windows), hurst_windows)
    if not warmup["sufficient"]:
        print(f"[1424] WARNING: warm-up shortfall on "
              f"{len(warmup['insufficient_datasets'])} dataset(s): "
              f"{', '.join(warmup['insufficient_datasets'])}. H is UNDEFINED on "
              f"their first scored bars, so the NaN bucket carries real trades. "
              f"NaN stays its own bucket (never 0.5) and holds the gate state.")
    else:
        print(f"[1424] warm-up OK: min lead {warmup['min_lead_bars']} bars before "
              f"each dataset's own earliest scored window "
              f"(need {warmup['required_bars']}).")

    print(f"[1424] computing rolling Hurst for {len(usable_datasets)}x"
          f"{len(hurst_windows)} (dataset, window) pairs...")
    hurst: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, "hurst_1424_rolling.npz")
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _hurst_key(dataset, hw):
        exchange_id, symbol, timeframe = dataset
        return f"{exchange_id}|{symbol}|{timeframe}|{hw}"

    def _hurst_job(job):
        dataset, hw = job
        key = _hurst_key(dataset, hw)
        frame = frames[dataset]
        first_needed = first_needed_by_ds[dataset]
        if key in cached and cache_entry_is_usable(
                cached.get(f"meta|{key}"), frame.index, first_needed):
            return job, pd.Series(cached[key], index=frame.index)
        return job, rolling_hurst(frame["close"], hw, first_needed=first_needed)

    jobs = [(ds, hw) for ds in usable_datasets for hw in hurst_windows]
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_hurst_job, jobs):
            hurst[job] = series
    if cache_path:
        arrays = {}
        for ds, hw in jobs:
            key = _hurst_key(ds, hw)
            arrays[key] = hurst[(ds, hw)].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(frames[ds].index,
                                               first_needed_by_ds[ds])
        np.savez_compressed(cache_path, **arrays)

    print(f"[1424] computing entry-ADX stamps for {len(usable_datasets)} datasets...")
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}

    print("[1424] computing symbol daily-return correlations...")
    rho_by_symbol = symbol_return_correlations(
        {ds: frames[ds] for ds in usable_datasets})

    units = [(family, exemplar, ds, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for ds in usable_datasets
             for wname in scored_windows
             if _cell_ok(ds, wname)]
    print(f"[1424] scoring {len(units)} legs "
          f"({len(hurst_windows) * 3} gated arms each)...")

    def _leg_job(unit):
        family, exemplar, ds, wname = unit
        by_window = {hw: hurst[(ds, hw)] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, ds, wname, frames[ds],
                         by_window, adx_stamps[ds],
                         verify_mirror=not args.no_mirror_check)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg["family"], lg["strategy"], lg["dataset"], lg["window"]))

    pooled = {}
    raw_counts = {}
    for family in families:
        rows = [t for lg in legs if lg["family"] == family for t in lg["trades"]]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)

    # Structural guarantee, not a promise: no primary trade may come from a
    # cell #1410 scored. Without this the primary hypothesis — chosen by
    # reading #1410's p-values — would be scored on its own selection data.
    for family in FAMILIES:
        for t in pooled[family]:
            if t["cohort"] != COHORT_PRIMARY:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if key in D_1410:
                raise AssertionError(f"primary cohort leaked a #1410 cell: {key}")

    print("[1424] sweeping configs and running both nulls on both targets...")
    configs = build_configs(legs, pooled, hurst_windows, rho_by_symbol,
                            args.n_perm, args.seed)
    configs = [c for c in configs if c["family"] in families]
    apply_bh_by_cohort(configs, alpha=ALPHA)

    print("[1424] measuring detection limits...")
    mde = measure_detection_limits(pooled, hurst_windows, args.n_perm_mde,
                                   args.seed)

    decision = decide_recommendation(configs, mde)

    buckets = {family: {str(hw): bucket_tables(pooled[family], hw)
                        for hw in hurst_windows}
               for family in FAMILIES}

    joint_hw = max(hurst_windows)
    joint = {}
    for family in FAMILIES:
        joint[family] = {
            "table": joint_adx_hurst_table(pooled[family], joint_hw),
            "verdict": joint_separation_verdict(
                pooled[family], joint_hw, n_perm=args.n_perm, seed=args.seed),
        }

    n_primary = sum(1 for c in configs if c["cohort"] == COHORT_PRIMARY)
    n_expl = sum(1 for c in configs if c["cohort"] == COHORT_EXPLORATORY)
    payload = {
        "schema_version": SCHEMA_VERSION,
        "issue": ISSUE,
        "pre_registered": {
            "families": {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES},
            "family_sense": dict(FAMILY_SENSE),
            "exemplar_close_overrides": EXEMPLAR_CLOSE_OVERRIDES,
            "buckets": list(BUCKETS),
            "hurst_windows": list(hurst_windows),
            "gate_pairs": {f: [list(p) for p in GATE_PAIRS[f]] for f in FAMILIES},
            "gate_initial_armed": GATE_INITIAL_ARMED,
            "sizing": {"gains": list(SIZING_GAINS), "clamp_lo": SIZING_CLAMP_LO,
                       "clamp_hi": SIZING_CLAMP_HI,
                       "nan_multiplier": SIZING_NAN_MULTIPLIER},
            "primary_config_id": PRIMARY_CONFIG_ID,
            "primary_config_ids": list(PRIMARY_CONFIG_IDS),
            "primary_family_size": PRIMARY_FAMILY_SIZE,
            "primary_target": PRIMARY_TARGET,
            "continuity_target": CONTINUITY_TARGET,
            "horizon_hours": HORIZON_HOURS,
            "interim_look_disclosure": INTERIM_LOOK_DISCLOSURE,
            "key_risk_prediction": KEY_RISK_PREDICTION,
            "feasibility_probes": [dict(pr) for pr in FEASIBILITY_PROBES],
            "window_owner": dict(WINDOW_OWNER),
            "dataset_windows": {
                f"{ex}={dataset_key(sym, tf)}": list(ws)
                for (ex, sym, tf), ws in sorted(DATASET_WINDOWS.items())},
            "history_since": dict(HISTORY_SINCE),
            "dataset_history_since": {
                f"{ex}={dataset_key(sym, tf)}": since
                for (ex, sym, tf), since in sorted(DATASET_HISTORY_SINCE.items())},
            "fetch_page_limit": dict(FETCH_PAGE_LIMIT),
            "min_suppressed_effective": MIN_SUPPRESSED_EFFECTIVE,
            "min_kept_effective": MIN_KEPT_EFFECTIVE,
            "return_tolerance_pp": RETURN_TOLERANCE_PP,
            "return_tolerance_frac": RETURN_TOLERANCE_FRAC,
            "held_out_min_fraction": HELD_OUT_MIN_FRACTION,
            "held_out_min_windows": HELD_OUT_MIN_WINDOWS,
            "alpha": ALPHA,
            "n_perm": args.n_perm,
            "n_perm_mde": args.n_perm_mde,
            "seed": args.seed,
            "min_offset_days": MIN_OFFSET_DAYS,
            "adx_period": ADX_PERIOD,
            "adx_split": ADX_SPLIT,
            "mde_eff_grid": [MDE_EFF_GRID_STEP, MDE_EFF_GRID_MAX,
                             MDE_EFF_REFINE_STEP],
            "mde_pp_grid": [MDE_PP_GRID_STEP, MDE_PP_GRID_MAX,
                            MDE_PP_REFINE_STEP],
            "windows": {k: list(WINDOWS[k]) for k in scored_windows},
            "primary_protocol_windows": list(PRIMARY_PROTOCOL_WINDOWS),
            "primary_protocol_min_windows": PRIMARY_PROTOCOL_MIN_WINDOWS,
            "primary_held_out_windows": list(PRIMARY_HELD_OUT_WINDOWS),
            "exploratory_protocol_windows": list(EXPLORATORY_PROTOCOL_WINDOWS),
            "exploratory_held_out_windows": list(EXPLORATORY_HELD_OUT_WINDOWS),
            "datasets": [dataset_key(qualified_symbol(ex, sym), tf)
                         for (ex, sym, tf) in usable_datasets],
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "scope": scope,
            "legs": len(legs),
            "gated_arms": sum(len(lg["gated"]) for lg in legs),
            "mirror_verified_legs": sum(1 for lg in legs if lg["mirror_verified"]),
            "raw_trades": raw_counts,
            "pooled_trades": {f: len(pooled[f]) for f in FAMILIES},
            "pooled_primary": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_PRIMARY)
                for f in FAMILIES},
            "pooled_exploratory": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_EXPLORATORY)
                for f in FAMILIES},
            "pooled_with_target": {
                f: sum(1 for t in pooled[f] if t.get("efficiency") is not None)
                for f in FAMILIES},
            "n_primary_configs": n_primary,
            "n_exploratory_configs": n_expl,
            "n_primary_significant": sum(
                1 for c in configs
                if c["cohort"] == COHORT_PRIMARY and c.get("bh_reject")),
            "n_exploratory_significant": sum(
                1 for c in configs
                if c["cohort"] == COHORT_EXPLORATORY and c.get("bh_reject")),
            "warmup": warmup,
            "coverage": coverage,
            "backfill": backfill,
            "symbol_correlations": {f"{a}|{b}": v
                                    for (a, b), v in sorted(rho_by_symbol.items())},
            "elapsed_sec": round(time.time() - started, 2),
        },
        "mde": mde,
        "buckets": buckets,
        "joint": joint,
        "configs": configs,
        "legs": [{k: v for k, v in lg.items() if k != "trades"} for lg in legs],
        "decision": {
            "verdict": decision["verdict"],
            "justification": decision["justification"],
            "validity_gate": decision["validity_gate"],
            "key_risk_held": decision["key_risk_held"],
            "families": {f: {"n_tested": d["n_tested"], "n_passing": d["n_passing"],
                             "winner": (d["winner"] or {}).get("config_id")}
                         for f, d in decision["families"].items()},
        },
    }

    with open(args.json_out, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=False)
        fh.write("\n")
    print(f"[1424] wrote {args.json_out}")

    payload_for_report = dict(payload)
    payload_for_report["decision"] = decision
    report = render_report(payload_for_report)
    if args.write_report:
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1424] wrote {args.report_out}")
    else:
        print(render_recommendation(decision, mde, configs))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
