"""Tests for amd_ifvg.py — including a look-ahead regression covering #732.

The bug being regressed: prior code selected the IFVG nearest to the *day's
final close*, so the signal at bar K depended on bars K+1..end-of-day. The
fix processes entry bar-by-bar, choosing the IFVG nearest to the current
bar's close. Verify: replacing future bars cannot change earlier signals.
"""

import numpy as np
import pandas as pd

from amd_ifvg import amd_ifvg_core


def _make_intraday_df(rows: list) -> pd.DataFrame:
    """Build an OHLCV DataFrame from a list of (hour, o, h, l, c) tuples at
    15-minute spacing on 2024-01-01 UTC. ``hour`` is purely for readability
    in test fixtures (it does not appear in the resulting frame)."""
    idx = pd.date_range("2024-01-01 00:00", periods=len(rows), freq="15min", tz="UTC")
    return pd.DataFrame(
        {
            "open":   [r[1] for r in rows],
            "high":   [r[2] for r in rows],
            "low":    [r[3] for r in rows],
            "close":  [r[4] for r in rows],
            "volume": [100.0] * len(rows),
        },
        index=idx,
    )


def _build_two_ifvg_setup() -> list:
    """Common prefix bars: Asian range, sweep, two bullish IFVGs (A near 100.4,
    B near 102.5), and one drift bar. Stops before the divergent tail."""
    rows = []
    # 00–08 UTC Asian: oscillate 99.5–100.5 (32 bars × 15m = 8 h)
    for i in range(32):
        mid = 100 + 0.5 * (1 if i % 2 else -1)
        rows.append((0, mid - 0.1, mid + 0.5, mid - 0.5, mid))
    # 08:00 sweep below 99
    rows.append((8, 99.5, 99.6, 98.7, 99.0))
    # IFVG_A: 3 candles forming gap [100.0, 100.8] (midpoint 100.4)
    rows.append((8, 99.5, 100.0, 99.4, 99.9))
    rows.append((8, 100.3, 100.9, 100.1, 100.7))
    rows.append((8, 100.5, 101.3, 100.8, 101.2))
    # IFVG_B: 3 candles forming gap [102.0, 103.0] (midpoint 102.5)
    rows.append((9, 101.5, 102.0, 101.4, 101.8))
    rows.append((9, 101.7, 102.8, 101.6, 102.5))
    rows.append((9, 102.3, 103.5, 103.0, 103.4))
    return rows


class TestLookahead:
    """Regression: the IFVG choice at entry bar K must not depend on bars > K."""

    def test_entry_bar_signal_independent_of_future_close(self):
        """At entry bar K, the chosen IFVG must be the one nearest to K's
        close. Two scenarios share an identical prefix through K but diverge
        afterwards. The pre-fix implementation evaluated ``latest_close`` from
        the last bar of the day; therefore in the full series the chosen IFVG
        flipped to whichever sat nearer the *day-final close*, mutating the
        signal at bar K. The fix uses bar K's close only.
        """
        common = _build_two_ifvg_setup()
        # Entry bar K: close 102.5, low 102.2 → touches IFVG_B [102.0, 103.0]
        # but NOT IFVG_A [100.0, 100.8] (low never reaches A).
        # IFVG_B mid (102.5) is exactly at this close → fix picks B.
        entry_bar = (9, 102.5, 102.8, 102.2, 102.5)

        # Tail A: late drift up to 105. Buggy code's latest_close ≈ 105 → still
        # picks B (mid 102.5 closer to 105 than A's 100.4) — same result as fix.
        tail_up = [(10, 105, 105.3, 104.7, 105.0)] * 6
        # Tail B: late drift down to 100. Buggy code's latest_close ≈ 100 →
        # picks A (mid 100.4 closer to 100 than B's 102.5). Entry bar B's H/L
        # do NOT touch IFVG_A, so buggy code yields NO signal at K — and may
        # later signal on a different bar that touches A.
        tail_down = [(10, 100, 100.3, 99.7, 100.0)] * 6

        df_up = _make_intraday_df(common + [entry_bar] + tail_up)
        df_dn = _make_intraday_df(common + [entry_bar] + tail_down)

        out_up = amd_ifvg_core(df_up)
        out_dn = amd_ifvg_core(df_dn)

        entry_idx = df_up.index[len(common)]  # bar K, same timestamp in both

        # Fix invariant: signal at K is identical regardless of bars > K.
        # The pre-fix code would: produce signal=1 at K under tail_up
        # (picks B → bar touches B), but no signal at K under tail_down
        # (picks A → bar misses A).
        assert out_up.loc[entry_idx, "signal"] == out_dn.loc[entry_idx, "signal"], (
            f"signal at entry bar {entry_idx} changed when only future bars varied: "
            f"up={out_up.loc[entry_idx,'signal']} dn={out_dn.loc[entry_idx,'signal']}"
        )
        # Sanity: at least one of the two must have actually fired here, else
        # the test would trivially pass with both zero.
        assert out_up.loc[entry_idx, "signal"] != 0, (
            "test setup produced no signal at K — adjust fixture so the bug "
            "regression actually exercises a signal-firing bar"
        )

    def test_truncation_invariant(self):
        """For every bar K with a signal in the full series, regenerating the
        signal from the prefix df[:K] must yield the same sign at K. Catches
        any look-ahead that *also* changes the truncated answer."""
        common = _build_two_ifvg_setup()
        tail = [(10, 105, 105.3, 104.7, 105.0)] * 6
        entry_bar = (9, 102.5, 102.8, 102.2, 102.5)
        df = _make_intraday_df(common + [entry_bar] + tail)

        full = amd_ifvg_core(df)
        signal_bars = full.index[full["signal"] != 0]
        assert len(signal_bars) >= 1

        for k in signal_bars:
            truncated = df.loc[:k]
            partial = amd_ifvg_core(truncated)
            assert partial.loc[k, "signal"] == full.loc[k, "signal"], (
                f"signal at {k} differs after truncation: "
                f"full={full.loc[k,'signal']} truncated={partial.loc[k,'signal']}"
            )


class TestSmoke:
    def test_short_df_returns_zeros(self):
        df = _make_intraday_df([(0, 100, 101, 99, 100)] * 2)
        out = amd_ifvg_core(df)
        assert (out["signal"] == 0).all()

    def test_no_asian_range_skips_day(self):
        df = _make_intraday_df([(0, 100, 100, 100, 100)] * 40)
        out = amd_ifvg_core(df)
        assert (out["signal"] == 0).all()

    def test_signal_in_valid_set(self):
        rows = [(0, 100, 100.5, 99.5, 100) for _ in range(64)]
        df = _make_intraday_df(rows)
        out = amd_ifvg_core(df)
        assert set(out["signal"].unique()).issubset({-1, 0, 1})
