from __future__ import annotations
import json
import os
import sys
from datetime import datetime, timezone
from typing import Optional
DEFAULT_CERT_PATH = 'backtest/research/regime_directional_certifications.json'
CERT_PATH_ENV = 'GO_TRADER_DIRECTIONAL_CERT_PATH'

def normalize_cert_asset(symbol: str) -> str:
    s = (symbol or '').strip().upper()
    if not s:
        return ''
    for sep in ('/', ':', '-', '_'):
        i = s.find(sep)
        if i > 0:
            s = s[:i]
            break
    return s

def _cert_key(asset: str, timeframe: str, classifier: str) -> str:
    return f"{normalize_cert_asset(asset)}|{(timeframe or '').strip()}|{(classifier or '').strip().lower()}"

def cert_path(path: Optional[str]=None) -> str:
    if path:
        return path
    env = os.environ.get(CERT_PATH_ENV, '').strip()
    return env or DEFAULT_CERT_PATH

def load_certifications(path: Optional[str]=None) -> dict:
    p = cert_path(path)
    try:
        with open(p) as fh:
            data = json.load(fh)
    except FileNotFoundError:
        return {}
    except (ValueError, OSError) as exc:
        print(f'[#1085][WARN] directional certification artifact {p!r} unreadable ({exc}) — failing closed: directional policies run default-off.', file=sys.stderr)
        return {}
    if int(data.get('schema_version', 0)) != 1:
        print(f'[#1085][WARN] directional certification artifact {p!r} has unsupported schema_version — failing closed.', file=sys.stderr)
        return {}
    out = {}
    for e in data.get('certified', []) or []:
        try:
            out[_cert_key(e['asset'], e['timeframe'], e['classifier'])] = e
        except (KeyError, TypeError):
            print(f'[#1085][WARN] skipping malformed certified entry in {p!r}.', file=sys.stderr)
    return out

def _parse_expiry(value: str) -> Optional[datetime]:
    if not value:
        return None
    try:
        return datetime.fromisoformat(str(value).replace('Z', '+00:00'))
    except ValueError:
        return None

def is_directional_certified(certs: dict, asset: str, timeframe: str, classifier: str, now: Optional[datetime]=None) -> bool:
    entry = certs.get(_cert_key(asset, timeframe, classifier))
    if not entry:
        return False
    exp = _parse_expiry(entry.get('expires_at', ''))
    if exp is not None:
        now = now or datetime.now(timezone.utc)
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
        if exp <= now:
            return False
    return True

def certified_states(certs: dict, asset: str, timeframe: str, classifier: str, now: Optional[datetime]=None) -> Optional[dict]:
    entry = certs.get(_cert_key(asset, timeframe, classifier))
    if not entry:
        return None
    exp = _parse_expiry(entry.get('expires_at', ''))
    if exp is not None:
        now = now or datetime.now(timezone.utc)
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
        if exp <= now:
            return None
    states = entry.get('states')
    return dict(states) if isinstance(states, dict) else {}

def backtest_classifier(regime_windows_spec: Optional[dict]) -> str:
    return 'composite' if regime_windows_spec else 'adx'

def _normalize_window_key(name: str) -> str:
    return (name or '').strip().lower()

def config_directional_classifier(regime_cfg: Optional[dict], sc: Optional[dict]) -> str:
    windows = (regime_cfg or {}).get('windows') or {}
    if not windows:
        return 'adx'
    key = _normalize_window_key((sc or {}).get('regime_directional_window'))
    if key in ('', 'default'):
        if 'medium' in windows:
            key = 'medium'
        else:
            names = sorted((_normalize_window_key(n) for n in windows))
            key = names[0] if names else ''
    for name, spec in windows.items():
        if _normalize_window_key(name) == key:
            c = str((spec or {}).get('classifier') or '').strip().lower()
            return c or 'adx'
    return 'adx'
