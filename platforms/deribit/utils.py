#!/usr/bin/env python3
"""
Deribit API utilities for fetching real option expiries and strikes.
"""

import sys
import requests
import traceback
from datetime import datetime, timezone, timedelta
from typing import List, Tuple, Optional, Dict, Any

DERIBIT_API_BASE = "https://www.deribit.com/api/v2"


def _format_instrument(underlying: str, option_type: str, strike: float, expiry_str: str) -> str:
    """Build Deribit instrument name, e.g. BTC-13MAR26-75000-C."""
    t = datetime.strptime(expiry_str, "%Y-%m-%d")
    day = t.strftime("%d")
    month = t.strftime("%b").upper()
    year = t.strftime("%y")
    opt_type = "C" if option_type.lower() == "call" else "P"
    return f"{underlying.upper()}-{day}{month}{year}-{int(strike)}-{opt_type}"


def fetch_available_expiries(underlying: str, min_dte: int = 7, max_dte: int = 60) -> List[Tuple[str, int]]:
    """
    Fetch available option expiries from Deribit within DTE range.
    Returns list of (expiry_date_str, dte) tuples sorted by DTE.
    """
    try:
        url = f"{DERIBIT_API_BASE}/public/get_instruments?currency={underlying}&kind=option&expired=false"
        resp = requests.get(url, timeout=10)
        resp.raise_for_status()
        data = resp.json()
        
        expiries = set()
        now = datetime.now(timezone.utc)
        
        for instrument in data.get("result", []):
            exp_ts = instrument.get("expiration_timestamp")
            if not exp_ts:
                continue
            
            exp_time = datetime.fromtimestamp(exp_ts / 1000, tz=timezone.utc)
            dte = (exp_time - now).days
            
            if min_dte <= dte <= max_dte:
                expiry_str = exp_time.strftime("%Y-%m-%d")
                expiries.add((expiry_str, dte))
        
        # Sort by DTE
        return sorted(list(expiries), key=lambda x: x[1])
    
    except Exception as e:
        print(f"Failed to fetch Deribit expiries: {e}", file=sys.stderr)
        return []


def find_closest_expiry(underlying: str, target_dte: int, max_tolerance_days: int = 7) -> Optional[Tuple[str, int]]:
    """
    Find the Deribit expiry closest to target DTE within tolerance.
    Returns (expiry_str, actual_dte) or None if nothing found within max_tolerance_days.
    Matches the 7-day tolerance enforced by the Go pricer in deribit.go findNearestExpiry().
    """
    expiries = fetch_available_expiries(underlying, min_dte=1, max_dte=365)
    if not expiries:
        return None

    # Find closest DTE
    best = min(expiries, key=lambda x: abs(x[1] - target_dte))
    expiry_str, actual_dte = best

    if abs(actual_dte - target_dte) > max_tolerance_days:
        print(
            f"No Deribit expiry within {max_tolerance_days}d of target {target_dte} DTE "
            f"(closest: {expiry_str} at {actual_dte} DTE, diff={abs(actual_dte - target_dte)}d)",
            file=sys.stderr,
        )
        return None

    return best


def fetch_available_strikes(underlying: str, expiry_str: str, option_type: str = "call") -> List[float]:
    """
    Fetch available strikes for a given expiry and option type.
    Returns list of strike prices.
    """
    try:
        url = f"{DERIBIT_API_BASE}/public/get_instruments?currency={underlying}&kind=option&expired=false"
        resp = requests.get(url, timeout=10)
        resp.raise_for_status()
        data = resp.json()
        
        # Parse target expiry - set to EOD to match Deribit timestamps
        target_time = datetime.strptime(expiry_str, "%Y-%m-%d").replace(
            hour=8, minute=0, second=0, microsecond=0, tzinfo=timezone.utc
        )
        target_day = target_time.date()
        
        strikes = set()
        opt_suffix = "-C" if option_type.lower() == "call" else "-P"
        
        for instrument in data.get("result", []):
            exp_ts = instrument.get("expiration_timestamp")
            if not exp_ts:
                continue
            
            # Compare dates only (not exact timestamps)
            exp_time = datetime.fromtimestamp(exp_ts / 1000, tz=timezone.utc)
            if exp_time.date() != target_day:
                continue
            
            if not instrument["instrument_name"].endswith(opt_suffix):
                continue
            
            strike = instrument.get("strike")
            if strike:
                strikes.add(strike)
        
        return sorted(list(strikes))
    
    except Exception as e:
        print(f"Failed to fetch strikes: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return []


def find_closest_strike(underlying: str, expiry_str: str, option_type: str, target_strike: float) -> Optional[float]:
    """
    Find the closest available strike to target on Deribit.
    """
    strikes = fetch_available_strikes(underlying, expiry_str, option_type)
    if not strikes:
        return None

    return min(strikes, key=lambda s: abs(s - target_strike))


def get_live_quote(underlying: str, option_type: str, strike: float, expiry_str: str) -> Optional[Dict[str, Any]]:
    """
    Fetch live option quote from Deribit public ticker (mark price, spot, Greeks) in one call.
    Returns dict with keys: mark_price, underlying_price, greeks (delta, gamma, theta, vega);
    or None if unavailable.
    """
    try:
        instrument = _format_instrument(underlying, option_type, strike, expiry_str)
        url = f"{DERIBIT_API_BASE}/public/ticker?instrument_name={instrument}"
        resp = requests.get(url, timeout=10)
        resp.raise_for_status()
        data = resp.json()
        result = data.get("result", {})
        mark_price = result.get("mark_price")
        if mark_price is None or mark_price <= 0:
            return None
        greeks = result.get("greeks") or {}
        return {
            "mark_price": float(mark_price),
            "underlying_price": float(result.get("underlying_price") or 0),
            "greeks": {
                "delta": float(greeks.get("delta", 0)),
                "gamma": float(greeks.get("gamma", 0)),
                "theta": float(greeks.get("theta", 0)),
                "vega": float(greeks.get("vega", 0)),
            },
        }
    except Exception as e:
        print(f"Failed to fetch live quote for {underlying} {option_type} {strike} {expiry_str}: {e}", file=sys.stderr)
        return None


def get_live_premium(underlying: str, option_type: str, strike: float, expiry_str: str) -> Optional[float]:
    """
    Fetch live option mark price from Deribit public ticker.
    Returns mark price in underlying terms (e.g. BTC per contract), or None if unavailable.
    """
    quote = get_live_quote(underlying, option_type, strike, expiry_str)
    return quote["mark_price"] if quote else None


if __name__ == "__main__":
    # Test
    if len(sys.argv) > 1:
        underlying = sys.argv[1]
    else:
        underlying = "BTC"
    
    print(f"Available expiries for {underlying} (7-60 DTE):")
    expiries = fetch_available_expiries(underlying, min_dte=7, max_dte=60)
    for exp, dte in expiries[:10]:
        print(f"  {exp} (DTE: {dte})")
    
    if expiries:
        exp_str, dte = expiries[0]
        print(f"\nAvailable strikes for {exp_str}:")
        strikes = fetch_available_strikes(underlying, exp_str, "call")
        print(f"  Calls: {strikes[:10]} ... ({len(strikes)} total)")
