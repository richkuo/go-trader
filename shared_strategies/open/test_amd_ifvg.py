
import numpy as np
import pandas as pd

from amd_ifvg import amd_ifvg_core, _hours_in_window, _session_local

LEGACY_UTC = dict(
    asian_start_hour=0, asian_end_hour=8,
    london_start_hour=8, london_end_hour=12,
    session_tz="UTC",
)


def _make_intraday_df(rows: list) -> pd.DataFrame:
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
    rows = []
    for i in range(32):
        mid = 100 + 0.5 * (1 if i % 2 else -1)
        rows.append((0, mid - 0.1, mid + 0.5, mid - 0.5, mid))
    rows.append((8, 99.5, 99.6, 98.7, 99.0))
    rows.append((8, 99.5, 100.0, 99.4, 99.9))
    rows.append((8, 100.3, 100.9, 100.1, 100.7))
    rows.append((8, 100.5, 101.3, 100.8, 101.2))
    rows.append((9, 101.5, 102.0, 101.4, 101.8))
    rows.append((9, 101.7, 102.8, 101.6, 102.5))
    rows.append((9, 102.3, 103.5, 103.0, 103.4))
    return rows


class TestLookahead:

    def test_entry_bar_signal_independent_of_future_close(self):
        common = _build_two_ifvg_setup()
        entry_bar = (9, 102.5, 102.8, 102.2, 102.5)

        tail_up = [(10, 105, 105.3, 104.7, 105.0)] * 6
        tail_down = [(10, 100, 100.3, 99.7, 100.0)] * 6

        df_up = _make_intraday_df(common + [entry_bar] + tail_up)
        df_dn = _make_intraday_df(common + [entry_bar] + tail_down)

        out_up = amd_ifvg_core(df_up, **LEGACY_UTC)
        out_dn = amd_ifvg_core(df_dn, **LEGACY_UTC)

        entry_idx = df_up.index[len(common)]

        assert out_up.loc[entry_idx, "signal"] == out_dn.loc[entry_idx, "signal"], (
            f"signal at entry bar {entry_idx} changed when only future bars varied: "
            f"up={out_up.loc[entry_idx,'signal']} dn={out_dn.loc[entry_idx,'signal']}"
        )
        assert out_up.loc[entry_idx, "signal"] != 0, (
            "test setup produced no signal at K — adjust fixture so the bug "
            "regression actually exercises a signal-firing bar"
        )

    def test_truncation_invariant(self):
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

    def test_simple_window(self):
        h = np.arange(24)
        m = _hours_in_window(h, 2, 5)
        assert set(h[m]) == {2, 3, 4}

    def test_end_midnight_is_24(self):
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

    def test_utc_instant_maps_to_dst_aware_local(self):
        winter = pd.DatetimeIndex(["2024-01-15 02:00"], tz="UTC")
        summer = pd.DatetimeIndex(["2024-07-15 01:00"], tz="UTC")
        lw = _session_local(winter, "America/New_York")
        ls = _session_local(summer, "America/New_York")
        assert lw.hour[0] == 21 and ls.hour[0] == 21
        naive = pd.DatetimeIndex(["2024-01-15 02:00"])
        assert _session_local(naive, "America/New_York").hour[0] == 21


def _make_utc_df_from_ny(ny_bars: list, day: str, tz="America/New_York") -> pd.DataFrame:
    idx_ny = pd.date_range(f"{day} 20:00", periods=len(ny_bars), freq="15min", tz=tz)
    idx_utc = idx_ny.tz_convert("UTC").tz_localize(None)
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
    return [
        (m - 0.1, m + 0.5, m - 0.5, m)
        for i in range(16)
        for m in (100 + 0.5 * (1 if i % 2 else -1),)
    ]


def _ny_london_bars() -> list:
    return [
        (99.5, 99.6, 98.7, 99.0),
        (99.5, 100.0, 99.4, 99.9),
        (100.3, 100.9, 100.1, 100.7),
        (100.5, 101.3, 100.8, 101.2),
        (100.4, 100.6, 100.0, 100.4),
        (100.6, 100.9, 100.3, 100.6),
        (100.6, 100.9, 100.3, 100.6),
        (100.6, 100.9, 100.3, 100.6),
    ]


def _build_ny_bullish_setup() -> list:
    drift = [(100.0, 100.2, 99.8, 100.0)] * 8
    return _ny_asian_bars() + drift + _ny_london_bars()


def _make_dst_crossing_df(tz="America/New_York") -> pd.DataFrame:
    asian = _ny_asian_bars()
    london = _ny_london_bars()
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

    def test_signal_fires_under_default_ny_windows(self):
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15")
        out = amd_ifvg_core(df)
        assert (out["signal"] == 1).any(), "expected a bullish signal under NY-canon windows"

    def test_winter_and_summer_setups_match(self):
        winter = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15"))
        summer = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-07-15"))

        ws = winter["signal"].to_numpy()
        ss = summer["signal"].to_numpy()
        assert np.array_equal(ws, ss), (
            f"DST changed the signal sequence: winter fired at {np.flatnonzero(ws)}, "
            f"summer at {np.flatnonzero(ss)}"
        )
        assert (ws == 1).any()

    def test_dst_shifts_utc_hour_of_signal(self):
        winter = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15"))
        summer = amd_ifvg_core(_make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-07-15"))
        wk = winter.index[winter["signal"] == 1][0]
        sk = summer.index[summer["signal"] == 1][0]
        assert ((wk.hour - sk.hour) % 24) == 1, (
            f"expected a 1h UTC shift across DST: winter {wk.hour}h, summer {sk.hour}h"
        )


class TestSessionDayWrap:

    def test_setup_straddling_midnight_fires(self):
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-03-10")
        out = amd_ifvg_core(df)
        assert (out["signal"] == 1).any(), (
            "setup straddling civil midnight failed to fire — session-day "
            "grouping likely split the Asian range from the London sweep"
        )


class TestTruncationDefaultPath:

    def _assert_truncation_invariant(self, df):
        full = amd_ifvg_core(df)
        signal_bars = full.index[full["signal"] != 0]
        assert len(signal_bars) >= 1, "fixture produced no signal to test against"
        for k in signal_bars:
            partial = amd_ifvg_core(df.loc[:k])
            assert partial.loc[k, "signal"] == full.loc[k, "signal"], (
                f"signal at {k} changed after truncation at K: "
                f"full={full.loc[k,'signal']} truncated={partial.loc[k,'signal']}"
            )

    def test_default_ny_path_truncation_invariant(self):
        df = _make_utc_df_from_ny(_build_ny_bullish_setup(), "2024-01-15")
        self._assert_truncation_invariant(df)

    def test_dst_boundary_truncation_invariant(self):
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
