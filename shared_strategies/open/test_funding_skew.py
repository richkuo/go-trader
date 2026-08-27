import numpy as np
import pandas as pd
from funding_skew import funding_skew_core

def make_ohlcv(closes, noise=0.5, tz='UTC'):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({'open': closes - noise * 0.3, 'high': closes + noise, 'low': closes - noise, 'close': closes, 'volume': np.full(n, 100.0)}, index=pd.date_range('2026-01-01', periods=n, freq='1h', tz=tz))

def make_short_squeeze():
    n = 400
    rng = np.random.RandomState(3)
    closes = 100 + np.cumsum(rng.randn(n)) * 0.05
    funding = rng.randn(n) * 2e-06
    funding[250:271] = -8e-05
    closes[245:300] = closes[245] + np.linspace(0, 6, 55)
    df = make_ohlcv(closes)
    df['funding_rate'] = funding
    return df

def make_long_breakdown():
    n = 400
    rng = np.random.RandomState(9)
    closes = 100 + np.cumsum(rng.randn(n)) * 0.05
    funding = rng.randn(n) * 2e-06
    funding[250:271] = 8e-05
    closes[245:300] = closes[245] - np.linspace(0, 6, 55)
    df = make_ohlcv(closes)
    df['funding_rate'] = funding
    return df

def test_columns_present():
    out = funding_skew_core(make_short_squeeze(), funding_window=96, z_entry=1.5, confirm_ema=20)
    for col in ('signal', 'position', 'funding_rate', 'funding_z'):
        assert col in out.columns

def test_empty_df_is_safe():
    df = pd.DataFrame(columns=['open', 'high', 'low', 'close', 'volume'])
    out = funding_skew_core(df)
    assert 'signal' in out.columns
    assert len(out) == 0

def test_no_funding_data_stays_flat():
    df = make_short_squeeze().drop(columns=['funding_rate'])
    out = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    assert (out['position'] == 0).all()

def test_crowded_shorts_with_price_confirmation_goes_long():
    out = funding_skew_core(make_short_squeeze(), funding_window=96, z_entry=1.5, confirm_ema=20)
    assert np.where(out['signal'].values == 1)[0].tolist() == [250]
    assert np.where(out['signal'].values == -1)[0].tolist() == [271]
    assert int((out['position'] == 1).sum()) == 21
    assert out['funding_z'].iloc[250] <= -1.5
    assert out['funding_rate'].iloc[250] < 0

def test_crowded_longs_with_breakdown_goes_short():
    out = funding_skew_core(make_long_breakdown(), funding_window=96, z_entry=1.5, confirm_ema=20)
    shorts = np.where(out['signal'].values == -1)[0]
    assert len(shorts) > 0 and shorts[0] == 250
    assert (out['position'] == -1).any()
    assert out['funding_z'].iloc[250] >= 1.5

def test_allow_short_false_blocks_shorts():
    out = funding_skew_core(make_long_breakdown(), funding_window=96, z_entry=1.5, confirm_ema=20, allow_short=False)
    assert not (out['position'] == -1).any()

def test_price_confirmation_required():
    df = make_short_squeeze()
    closes = df['close'].values.copy()
    closes[245:300] = closes[245] - np.linspace(0, 6, 55)
    for col, off in (('open', -0.15), ('high', 0.5), ('low', -0.5), ('close', 0.0)):
        df[col] = closes + off
    out = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    assert not (out['position'] == 1).any()

def test_min_abs_rate_floor_blocks_noise_extremes():
    df = make_short_squeeze()
    df['funding_rate'] = df['funding_rate'] / 100.0
    out = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    assert (out['position'] == 0).all()

def test_funding_records_match_column():
    df = make_short_squeeze()
    recs = [{'rate': float(df['funding_rate'].iloc[i]), 'time': int(df.index[i].timestamp() * 1000)} for i in range(len(df))]
    via_column = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    via_records = funding_skew_core(df.drop(columns=['funding_rate']), funding_window=96, z_entry=1.5, confirm_ema=20, funding_records=recs)
    assert (via_records['signal'].values == via_column['signal'].values).all()

def test_records_alignment_is_backward_only():
    df = make_ohlcv([100.0] * 5).drop(columns=[], errors='ignore')
    recs = [{'rate': 5e-05, 'time': int(df.index[2].timestamp() * 1000) + 1}]
    out = funding_skew_core(df, funding_records=recs)
    assert np.isnan(out['funding_rate'].iloc[2])
    assert out['funding_rate'].iloc[3] == 5e-05

def test_tz_naive_index_matches_utc():
    df = make_short_squeeze()
    naive = df.copy()
    naive.index = naive.index.tz_localize(None)
    a = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    b = funding_skew_core(naive, funding_window=96, z_entry=1.5, confirm_ema=20)
    assert (a['signal'].values == b['signal'].values).all()

def test_no_lookahead_future_perturbation():
    df = make_short_squeeze()
    full = funding_skew_core(df, funding_window=96, z_entry=1.5, confirm_ema=20)
    cut = 260
    pert = df.copy()
    pert.iloc[cut:, 0:5] = pert.iloc[cut:, 0:5] * 1.5
    pert.loc[pert.index[cut:], 'funding_rate'] = 9e-05
    out = funding_skew_core(pert, funding_window=96, z_entry=1.5, confirm_ema=20)
    assert (full['signal'].iloc[:cut].values == out['signal'].iloc[:cut].values).all()
    assert (full['position'].iloc[:cut].values == out['position'].iloc[:cut].values).all()
