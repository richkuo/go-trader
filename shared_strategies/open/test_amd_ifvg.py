"""Tests for amd_ifvg.py.

Covers three concerns:
1. The #732 look-ahead regression: a signal at bar K must never depend on
   bars > K (verified on the explicit-UTC-override path, which is also the
   retained legacy/override surface).
2. DST-aware civil sessions (#1023): windows are anchored to ``session_tz``
   (default America/New_York) so the same civil ICT session fires identically
   whether it lands in EST or EDT, even though its UTC hours shift by one.
3. Session-day grouping across civil midnight: the Asian range (prior evening)
   and the London manipulation that follows it next morning belong to one
   logical ICT day.
"""

import numpy as np
import pandas as pd

from amd_ifvg import amd_ifvg_core, _hours_in_window, _session_local

# Legacy UTC-hour windows: reproduce the pre-#1023 behaviour exactly so the
# look-ahead fixtures (built on a UTC grid at hours 0-12) remain valid. Passing
# these also exercises the retained explicit-hour / session_tz override path.
LEGACY_UTC = dict(
    asian_start_hour=0, asian_end_hour=8,
    london_start_hour=8, london_end_hour=12,
    session_tz="UTC",
)


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
    """Regression: the IFVG choice at entry bar K must not depend on bars > K.

    Run on the explicit legacy UTC-hour window so the simple 0–12 UTC fixtures
    remain valid; the look-ahead invariant is timezone-independent.
    """

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

        out_up = amd_ifvg_core(df_up, **LEGACY_UTC)
        out_dn = amd_ifvg_core(df_dn, **LEGACY_UTC)

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

        full = amd_ifvg_core(df, **LEGACY_UTC)
        signal_bars = full.index[full["signal"] != 0]
        assert len(signal_bars) >= 1

        for k in signal_bars:
            truncated = df.loc[:k]
            partial = amd_ifvg_core(truncated, **LEGACY_UTC)
            assert partial.loc[k, "signal"] == full.loc[k, "signal"], (
                f"signal at {k} differs after truncation: "
                f"full={full.loc[k,'signal']} truncated={partial.loc[k,'signal']}"
            )


class TestHoursInWindow:
    """Unit tests for the half-open, midnight-wrap-aware hour mask."""

    def test_simple_window(self):
        h = np.arange(24)
        m = _hours_in_window(h, 2, 5)
        assert set(h[m]) == {2, 3, 4}

    def test_end_midnight_is_24(self):
        # 20:00–00:00 must select the evening hours, not the empty set.
        h = np.arange(24)
        m = _hours_in_window(h, 20, 0)
        assert set(h[m]) == {20, 21, 22, 23}

    def test_wraps_past_midnight(self):
        h = np.arange(24)
        m = _hours_in_window(h, 22, 2)
        assert set(h[m]) == {22, 23, 0, 1}

    def test_legacy_window_unchanged(self):
        h = np.arange(24)
        assert set(h[_hours_in_window(h, 0, 8)]) == set(range(8))
        assert set(h[_hours_in_window(h, 8, 12)]) == {8, 9, 10, 11}


class TestSessionTZ:
    """DST-aware civil-time conversion (#1023)."""

    def test_utc_instant_maps_to_dst_aware_local(self):
        # Same civil clock target (NY 21:00) lands on different UTC hours in
        # winter (EST, UTC-5) vs summer (EDT, UTC-4).
        winter = pd.DatetimeIndex(["2024-01-15 02:00"], tz="UTC")  # 21:00 EST (prev day)
        summer = pd.DatetimeIndex(["2024-07-15 01:00"], tz="UTC")  # 21:00 EDT (prev day)
        lw = _session_local(winter, "America/New_York")
        ls = _session_local(summer, "America/New_York")
        assert lw.hour[0] == 21 and ls.hour[0] == 21
        # tz-naive UTC index is treated as UTC (backtest loader path).
        naive = pd.DatetimeIndex(["2024-01-15 02:00"])  # no tz
        assert _session_local(naive, "America/New_York").hour[0] == 21


def _make_utc_df_from_ny(ny_bars: list, day: str, tz="America/New_York") -> pd.DataFrame:
    """Build a UTC-indexed OHLCV frame from bars specified in civil NY time.

    ``ny_bars`` is a list of (o, h, l, c) at 15-minute spacing beginning at
    ``{day} 20:00`` civil ``tz`` time (the Asian open / session-day anchor).
    Returns the frame with the UTC index the strategy consumes, so DST is
    exercised end-to-end."""
    idx_ny = pd.date_range(f"{day} 20:00", periods=len(ny_bars), freq="15min", tz=tz)
    idx_utc = idx_ny.tz_convert("UTC").tz_localize(None)  # tz-naive UTC (backtest shape)
    return pd.DataFrame(
        {
            "open":   [b[0] for b in ny_bars],
            "high":   [b[1] for b in ny_bars],
            "low":    [b[2] for b in ny_bars],
            "close":  [b[3] for b in ny_bars],
            "volume": [100.0] * len(ny_bars),
        },
        index=idx_utc,
    )


def _ny_asian_bars() -> list:
    """Asian range, 16 bars (4 h) → range ≈ [99.0, 101.0]."""
    return [
        (m - 0.1, m + 0.5, m - 0.5, m)
        for i in range(16)
        for m in (100 + 0.5 * (1 if i % 2 else -1),)
    ]


def _ny_london_bars() -> list:
    """London sweep below the Asian low, bullish IFVG, retrace entry, drift
    (8 bars). Designed to fire signal=+1 once placed in the London window."""
    return [
        (99.5, 99.6, 98.7, 99.0),     # sweep below 99.0
        (99.5, 100.0, 99.4, 99.9),    # IFVG c0: high 100.0
        (100.3, 100.9, 100.1, 100.7), # IFVG c1 (displacement)
        (100.5, 101.3, 100.8, 101.2), # IFVG c2: low 100.8 → gap [100.0, 100.8]
        (100.4, 100.6, 100.0, 100.4), # retrace entry: dips in, closes inside
        (100.6, 100.9, 100.3, 100.6),
        (100.6, 100.9, 100.3, 100.6),
        (100.6, 100.9, 100.3, 100.6),
    ]


def _build_ny_bullish_setup() -> list:
    """A bullish AMD+IFVG setup on a continuous 15m NY-civil grid from 20:00 ET:
      20:00–23:45  Asian range        (16 bars)
      00:00–01:45  inter-session drift  (8 bars, pushes the sweep to 02:00 ET)
      02:00+       London sweep + bullish IFVG + retrace entry.
    Fires signal=+1 under the default NY-canon windows; crosses civil midnight."""
    drift = [(100.0, 100.2, 99.8, 100.0)] * 8
    return _ny_asian_bars() + drift + _ny_london_bars()


def _make_dst_crossing_df(tz="America/New_York") -> pd.DataFrame:
    """Same bullish setup, but the Asian range (evening of 2024-11-02) and the
    London phase (early 2024-11-03) bracket the US fall-back DST transition
    (02:00 EDT → 01:00 EST). The two phases are built as separate civil-time
    grids — avoiding the ambiguous repeated 01:00 hour — so the *session*
    spans the boundary while the bar placement stays unambiguous. Fires +1."""
    asian = _ny_asian_bars()                     # 20:00–23:45 EDT, 2024-11-02
    london = _ny_london_bars()                   # 02:30–04:15 EST, 2024-11-03
    a_idx = pd.date_range("2024-11-02 20:00", periods=len(asian), freq="15min", tz=tz)
    l_idx = pd.date_range("2024-11-03 02:30", periods=len(london), freq="15min", tz=tz)
    idx = a_idx.append(l_idx).tz_convert("UTC").tz_localize(None)
    rows = asian + london
    return pd.DataFrame(
        {
            "open":   [b[0] for b in rows],
            "high":   [b[1] for b in rows],
            "low":    [b[2] for b in rows],
            "close":  [b[3] for b in rows],
            "volume": [100.0] * len(rows),
        },
        index=idx,
    )


class TestDSTInvariance:
    """The same civil-time setup must produce identical signals in EST and
    EDT — the core #1023 guarantee. Under the old UTC-hour code the summer
    setup would fall outside the fixed UTC windows and behave differently."""

    def test_signal_fires_under_default_ny_windows(self):
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15")
        out = amd_ifvg_core(df)  # default America/New_York windows
        assert (out["signal"] == 1).any(), "expected a bullish signal under NY-canon windows"

    def test_winter_and_summer_setups_match(self):
        # Identical civil-time layout, one in January (EST), one in July (EDT).
        winter = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15"))
        summer = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-07-15"))

        # Compare signals by civil-time offset (position), not UTC timestamp:
        # the UTC index differs by an hour across DST but the civil sequence is
        # identical, so the strategy must fire at the same positions.
        ws = winter["signal"].to_numpy()
        ss = summer["signal"].to_numpy()
        assert np.array_equal(ws, ss), (
            f"DST changed the signal sequence: winter fired at {np.flatnonzero(ws)}, "
            f"summer at {np.flatnonzero(ss)}"
        )
        assert (ws == 1).any()

    def test_dst_shifts_utc_hour_of_signal(self):
        # Same civil setup → the firing bar's UTC hour differs by exactly one
        # hour between EST and EDT, proving the window tracked civil time.
        winter = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15"))
        summer = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-07-15"))
        wk = winter.index[winter["signal"] == 1][0]
        sk = summer.index[summer["signal"] == 1][0]
        # winter EST = UTC-5, summer EDT = UTC-4 → summer UTC hour is one less.
        assert ((wk.hour - sk.hour) % 24) == 1, (
            f"expected a 1h UTC shift across DST: winter {wk.hour}h, summer {sk.hour}h"
        )


class TestSessionDayWrap:
    """The Asian range (prior evening) and the London manipulation next morning
    must group into one session day across civil midnight."""

    def test_setup_straddling_midnight_fires(self):
        # _build_ny_bullish_setup spans 20:00 ET → ~03:30 ET next day, i.e. it
        # crosses civil midnight. A correct session-day anchor keeps them
        # together; a naive calendar-day split would orphan the London phase.
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-03-10")
        out = amd_ifvg_core(df)
        assert (out["signal"] == 1).any(), (
            "setup straddling civil midnight failed to fire — session-day "
            "grouping likely split the Asian range from the London sweep"
        )


class TestTruncationDefaultPath:
    """#732 truncation invariance on the PRODUCTION default (America/New_York)
    path, not only the legacy UTC override. Session bucketing is a function of
    the index alone, so the invariant is timezone-independent — these lock it in
    against future session-derivation edits while amd_ifvg stays live-loadable."""

    def _assert_truncation_invariant(self, df):
        full = amd_ifvg_core(df)  # default NY-canon windows
        signal_bars = full.index[full["signal"] != 0]
        assert len(signal_bars) >= 1, "fixture produced no signal to test against"
        for k in signal_bars:
            partial = amd_ifvg_core(df.loc[:k])
            assert partial.loc[k, "signal"] == full.loc[k, "signal"], (
                f"signal at {k} changed after truncation at K: "
                f"full={full.loc[k,'signal']} truncated={partial.loc[k,'signal']}"
            )

    def test_default_ny_path_truncation_invariant(self):
        # (a) default NY path; (c) the setup crosses civil midnight (Asian
        # evening → London next morning) within one session day.
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15")
        self._assert_truncation_invariant(df)

    def test_dst_boundary_truncation_invariant(self):
        # (b) the session brackets the US fall-back DST transition; (c) it also
        # crosses civil midnight. Truncating at the entry bar must not change it.
        self._assert_truncation_invariant(_make_dst_crossing_df())


class TestSmoke:
    def test_short_df_returns_zeros(self):
        df = _make_intraday_df([(0, 100, 101, 99, 100)] * 2)
        out = amd_ifvg_core(df)
        assert (out["signal"] == 0).all()

    def test_no_asian_range_skips_day(self):
        df = _make_utc_df_from_ny([(100, 100, 100, 100)] * 40, "2024-01-15")
        out = amd_ifvg_core(df)
        assert (out["signal"] == 0).all()

    def test_signal_in_valid_set(self):
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15")
        out = amd_ifvg_core(df)
        assert set(out["signal"].unique()).issubset({-1, 0, 1})
