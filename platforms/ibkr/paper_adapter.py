import math
from datetime import datetime, timezone, timedelta
from typing import Optional, Dict, List, Tuple

def norm_cdf(x: float) -> float:
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))

def black_scholes(spot: float, strike: float, dte_days: float, vol: float, risk_free: float=0.05, option_type: str='call') -> float:
    if dte_days <= 0 or vol <= 0 or spot <= 0:
        if option_type == 'call':
            return max(spot - strike, 0)
        return max(strike - spot, 0)
    t = dte_days / 365.0
    d1 = (math.log(spot / strike) + (risk_free + 0.5 * vol ** 2) * t) / (vol * math.sqrt(t))
    d2 = d1 - vol * math.sqrt(t)
    if option_type == 'call':
        return spot * norm_cdf(d1) - strike * math.exp(-risk_free * t) * norm_cdf(d2)
    return strike * math.exp(-risk_free * t) * norm_cdf(-d2) - spot * norm_cdf(-d1)

def bs_greeks(spot: float, strike: float, dte_days: float, vol: float, risk_free: float=0.05, option_type: str='call') -> dict:
    if dte_days <= 0 or vol <= 0 or spot <= 0:
        return {'delta': 0, 'gamma': 0, 'theta': 0, 'vega': 0}
    t = dte_days / 365.0
    sqrt_t = math.sqrt(t)
    d1 = (math.log(spot / strike) + (risk_free + 0.5 * vol ** 2) * t) / (vol * sqrt_t)
    d2 = d1 - vol * sqrt_t
    pdf_d1 = math.exp(-0.5 * d1 ** 2) / math.sqrt(2 * math.pi)
    if option_type == 'call':
        delta = norm_cdf(d1)
    else:
        delta = norm_cdf(d1) - 1
    gamma = pdf_d1 / (spot * vol * sqrt_t)
    vega = spot * pdf_d1 * sqrt_t / 100
    theta_annual = -(spot * pdf_d1 * vol) / (2 * sqrt_t) - risk_free * strike * math.exp(-risk_free * t) * (norm_cdf(d2) if option_type == 'call' else norm_cdf(-d2))
    theta = theta_annual / 365
    return {'delta': round(delta, 4), 'gamma': round(gamma, 6), 'theta': round(theta, 2), 'vega': round(vega, 2)}

def make_crypto_option_contract(underlying: str, strike: float, expiry: str, option_type: str, exchange: str='CME'):
    from ib_insync import FuturesOption
    symbol_map = {'BTC': 'MBT', 'ETH': 'MET'}
    symbol = symbol_map.get(underlying, underlying)
    expiry_str = expiry.replace('-', '')[:8]
    right = 'C' if option_type.lower() == 'call' else 'P'
    contract = FuturesOption(symbol=symbol, lastTradeDateOrContractMonth=expiry_str, strike=strike, right=right, exchange=exchange, currency='USD')
    return contract

def make_crypto_futures_contract(underlying: str, expiry: str='', exchange: str='CME'):
    from ib_insync import Future
    symbol_map = {'BTC': 'MBT', 'ETH': 'MET'}
    symbol = symbol_map.get(underlying, underlying)
    contract = Future(symbol=symbol, lastTradeDateOrContractMonth=expiry, exchange=exchange, currency='USD')
    return contract

class IBKRConnection:

    def __init__(self, host: str='127.0.0.1', port: int=4002, client_id: int=1):
        self.host = host
        self.port = port
        self.client_id = client_id
        self.ib = None

    def connect(self):
        from ib_insync import IB
        self.ib = IB()
        self.ib.connect(self.host, self.port, clientId=self.client_id)
        return self.ib

    def disconnect(self):
        if self.ib and self.ib.isConnected():
            self.ib.disconnect()

    def is_connected(self) -> bool:
        return self.ib is not None and self.ib.isConnected()

class IBKRPaperAdapter:
    MULTIPLIERS = {'BTC': 0.1, 'ETH': 0.5}
    TICK_SIZES = {'BTC': 5.0, 'ETH': 0.25}

    def __init__(self):
        self.positions = {}

    def get_multiplier(self, underlying: str) -> float:
        return self.MULTIPLIERS.get(underlying, 1.0)

    def get_contract_value(self, underlying: str, spot_price: float) -> float:
        return spot_price * self.get_multiplier(underlying)

    def estimate_premium(self, underlying: str, spot: float, strike: float, dte: int, vol: float, option_type: str) -> dict:
        bs_price = black_scholes(spot, strike, dte, vol, option_type=option_type)
        greeks = bs_greeks(spot, strike, dte, vol, option_type=option_type)
        multiplier = self.get_multiplier(underlying)
        premium_usd = bs_price * multiplier
        return {'premium_per_unit': round(bs_price, 2), 'premium_usd': round(premium_usd, 2), 'multiplier': multiplier, 'greeks': greeks}

    def get_available_strikes(self, underlying: str, spot: float, strike_range_pct: float=0.15) -> dict:
        tick = self.TICK_SIZES.get(underlying, 100)
        if underlying == 'BTC':
            intervals = [500, 1000, 2500, 5000]
            interval = 1000 if spot > 50000 else 500
        else:
            intervals = [25, 50, 100]
            interval = 50 if spot > 1000 else 25
        low = spot * (1 - strike_range_pct)
        high = spot * (1 + strike_range_pct)
        strikes = []
        strike = math.floor(low / interval) * interval
        while strike <= high:
            strikes.append(strike)
            strike += interval
        return {'underlying': underlying, 'spot': spot, 'interval': interval, 'strikes': strikes}

    def get_available_expiries(self, days_out: int=90) -> List[str]:
        now = datetime.now(timezone.utc)
        expiries = []
        for i in range(1, 6):
            d = now + timedelta(days=i)
            while d.weekday() != 4:
                d += timedelta(days=1)
            if d not in [datetime.strptime(e, '%Y-%m-%d').replace(tzinfo=timezone.utc) for e in expiries]:
                expiries.append(d.strftime('%Y-%m-%d'))
        for month_offset in range(1, 4):
            year = now.year
            month = now.month + month_offset
            if month > 12:
                month -= 12
                year += 1
            if month == 12:
                last_day = datetime(year + 1, 1, 1, tzinfo=timezone.utc) - timedelta(days=1)
            else:
                last_day = datetime(year, month + 1, 1, tzinfo=timezone.utc) - timedelta(days=1)
            while last_day.weekday() != 4:
                last_day -= timedelta(days=1)
            exp_str = last_day.strftime('%Y-%m-%d')
            if exp_str not in expiries:
                expiries.append(exp_str)
        return sorted(expiries)

def get_spot_price_ibkr(underlying: str) -> float:
    try:
        import ccxt
        exchange = ccxt.binanceus({'enableRateLimit': True})
        symbol = f'{underlying}/USDT'
        ticker = exchange.fetch_ticker(symbol)
        return ticker['last']
    except Exception as e:
        print(f'Spot price fetch failed for {underlying}: {e}', file=__import__('sys').stderr)
        return 0

def calc_vol_and_iv_rank(underlying: str) -> Tuple[float, float]:
    try:
        import ccxt
        exchange = ccxt.binanceus({'enableRateLimit': True})
        symbol = f'{underlying}/USDT'
        ohlcv = exchange.fetch_ohlcv(symbol, '1d', limit=90)
        if not ohlcv or len(ohlcv) < 30:
            return (0.5, 50.0)
        closes = [c[4] for c in ohlcv]
        returns = [(closes[i] - closes[i - 1]) / closes[i - 1] for i in range(1, len(closes))]
        recent_vol = math.sqrt(sum((r ** 2 for r in returns[-14:])) / 14) * math.sqrt(365)
        hist_vol = math.sqrt(sum((r ** 2 for r in returns)) / len(returns)) * math.sqrt(365)
        iv_rank = min(max(recent_vol / max(hist_vol, 0.001) * 50, 0), 100)
        return (recent_vol, round(iv_rank, 1))
    except Exception as e:
        print(f'Vol calc failed: {e}', file=__import__('sys').stderr)
        return (0.5, 50.0)
