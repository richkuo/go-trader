"""Evidence-gated directional certification — backtest consumer (#1085).

Mirror of scheduler/regime_directional_certification.go for backtest/live PARITY:
the backtester must honor regime_directional_policy only where the SAME
per-(asset, timeframe, classifier) certification passes that the live daemon
checks, so a backtest can never show a directional edge the live path suppresses.

Single source of truth for the statistical test is the Python research harness
(regime_1076_certify.py), which emits the artifact consumed here AND by Go. The
artifact currently certifies NOTHING (#1076 negative result), so every
directional policy is default-off in both backtest and live.

Fail-closed: a missing/malformed/expired certification yields "not certified"
(base direction), never a wrong-side bet. Keep normalize_cert_asset and the key
shape byte-identical to the Go side.
"""
from __future__ import annotations

import json
import os
import sys
from datetime import datetime, timezone
from typing import Optional

DEFAULT_CERT_PATH = "backtest/research/regime_directional_certifications.json"
CERT_PATH_ENV = "GO_TRADER_DIRECTIONAL_CERT_PATH"


def normalize_cert_asset(symbol: str) -> str:
    """Reduce a symbol to its base asset: strip a quote/perp suffix, upper-case.
    "BTC/USDT" -> "BTC", "btc" -> "BTC", "BTC-PERP" -> "BTC". Mirrors the Go
    normalizeCertAsset so both sides key identically."""
    s = (symbol or "").strip().upper()
    if not s:
        return ""
    for sep in ("/", ":", "-", "_"):
        i = s.find(sep)
        if i > 0:
            s = s[:i]
            break
    return s


def _cert_key(asset: str, timeframe: str, classifier: str) -> str:
    return (
        f"{normalize_cert_asset(asset)}|"
        f"{(timeframe or '').strip()}|"
        f"{(classifier or '').strip().lower()}"
    )


def cert_path(path: Optional[str] = None) -> str:
    if path:
        return path
    env = os.environ.get(CERT_PATH_ENV, "").strip()
    return env or DEFAULT_CERT_PATH


def load_certifications(path: Optional[str] = None) -> dict:
    """Load the artifact into a {cert_key: entry} index. Fail-closed: a missing
    or malformed artifact yields an empty index (nothing certified) with a
    stderr warning — never an exception that would break an unrelated backtest,
    mirroring the live daemon's fail-closed load."""
    p = cert_path(path)
    try:
        with open(p) as fh:
            data = json.load(fh)
    except FileNotFoundError:
        return {}
    except (ValueError, OSError) as exc:
        print(f"[#1085][WARN] directional certification artifact {p!r} unreadable "
              f"({exc}) — failing closed: directional policies run default-off.",
              file=sys.stderr)
        return {}
    if int(data.get("schema_version", 0)) != 1:
        print(f"[#1085][WARN] directional certification artifact {p!r} has "
              f"unsupported schema_version — failing closed.", file=sys.stderr)
        return {}
    out = {}
    for e in data.get("certified", []) or []:
        try:
            out[_cert_key(e["asset"], e["timeframe"], e["classifier"])] = e
        except (KeyError, TypeError):
            print(f"[#1085][WARN] skipping malformed certified entry in {p!r}.",
                  file=sys.stderr)
    return out


def _parse_expiry(value: str) -> Optional[datetime]:
    if not value:
        return None
    try:
        return datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None


def is_directional_certified(
    certs: dict, asset: str, timeframe: str, classifier: str,
    now: Optional[datetime] = None,
) -> bool:
    """True iff (asset, timeframe, classifier) has a present, non-expired
    certification. Fail-closed everywhere else."""
    entry = certs.get(_cert_key(asset, timeframe, classifier))
    if not entry:
        return False
    exp = _parse_expiry(entry.get("expires_at", ""))
    if exp is not None:
        now = now or datetime.now(timezone.utc)
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
        if exp <= now:
            return False
    return True


def backtest_classifier(regime_windows_spec: Optional[dict]) -> str:
    """The regime classifier the BACKTESTER actually applies: composite when a
    regime_windows_spec is configured (#1058), else the legacy single-lookback
    ADX. Certification is checked against this so the gate matches what the
    backtest computes."""
    return "composite" if regime_windows_spec else "adx"
