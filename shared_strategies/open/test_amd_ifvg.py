
import pandas as pd

from shared_strategies.open.conftest import load_module

_AMD_IFVG = load_module("_amd_ifvg_test", __file__.replace("test_amd_ifvg.py", "amd_ifvg.py"))
amd_ifvg_core = _AMD_IFVG.amd_ifvg_core
_hours_in_window = _AMD_IFVG._hours_in_window
_session_local = _AMD_IFVG._session_local

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

    def test_dst_boundary_truncation_invariant(self):
        self._assert_truncation_invariant(_make_dst_crossing_df())
