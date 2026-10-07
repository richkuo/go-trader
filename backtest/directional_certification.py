from __future__ import annotations

import json
import os
import sys
from datetime import datetime, timezone
from typing import Optional

DEFAULT_CERT_PATH = "backtest/research/regime_directional_certifications.json"
CERT_PATH_ENV = "GO_TRADER_DIRECTIONAL_CERT_PATH"


def normalize_cert_asset(symbol: str) -> str:
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


_CERT_TOP_KEYS = {
    "schema_version", "generated_at", "generator", "source_evidence",
    "criteria", "default_ttl_days", "certified",
}
_CERT_ENTRY_KEYS = {
    "asset", "timeframe", "classifier", "generated_at", "expires_at", "states",
}
_CERT_DIRECTIONS = {"long", "short"}


class CertificationInvalid(ValueError):
    """The artifact is not a set Go would load. The whole file is rejected."""


def parse_certifications_strict(data: dict) -> dict:
    """Load a certification artifact the way LoadDirectionalCertSet does.

    A malformed entry rejects the whole artifact. This does not skip entries
    the way load_certifications does, and a hash match is not this check.
    """
    if not isinstance(data, dict):
        raise CertificationInvalid("certification artifact must be a JSON object")
    unknown = sorted(set(data) - _CERT_TOP_KEYS)
    if unknown:
        raise CertificationInvalid(f"unknown certification keys: {unknown}")
    version = data.get("schema_version")
    if isinstance(version, bool) or version != 1:
        raise CertificationInvalid(
            f"unsupported schema_version {version!r} (want 1)")
    entries = data.get("certified", [])
    if entries is None:
        entries = []
    if not isinstance(entries, list):
        raise CertificationInvalid("certified must be a list")
    out = {}
    for i, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise CertificationInvalid(f"certified[{i}] must be an object")
        extra = sorted(set(entry) - _CERT_ENTRY_KEYS)
        if extra:
            raise CertificationInvalid(f"certified[{i}] has unknown keys: {extra}")
        asset = normalize_cert_asset(str(entry.get("asset") or ""))
        timeframe = str(entry.get("timeframe") or "").strip()
        classifier = str(entry.get("classifier") or "").strip().lower()
        if not asset or not timeframe or not classifier:
            raise CertificationInvalid(
                f"certified[{i}] missing asset/timeframe/classifier")
        if "generated_at" in entry and entry["generated_at"] not in (None, ""):
            _require_cert_time(entry["generated_at"], f"certified[{i}].generated_at")
        expires = None
        if "expires_at" in entry and entry["expires_at"] not in (None, ""):
            expires = _require_cert_time(entry["expires_at"], f"certified[{i}].expires_at")
        states = entry.get("states") or {}
        if not isinstance(states, dict):
            raise CertificationInvalid(f"certified[{i}].states must be an object")
        clean = {}
        for label, direction in states.items():
            side = str(direction or "").strip().lower()
            if side not in _CERT_DIRECTIONS:
                raise CertificationInvalid(
                    f"certified[{i}].states[{label!r}] must be 'long' or 'short' "
                    f"(got {direction!r})")
            clean[str(label)] = side
        out[_cert_key(asset, timeframe, classifier)] = {
            "asset": asset,
            "timeframe": timeframe,
            "classifier": classifier,
            "expires_at": expires.isoformat().replace("+00:00", "Z") if expires else "",
            "states": clean,
        }
    return out


def _require_cert_time(value, label: str):
    if not isinstance(value, str) or not value.strip():
        raise CertificationInvalid(f"{label} must be an RFC3339 timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise CertificationInvalid(f"{label} is not a timestamp: {exc}") from exc
    if parsed.tzinfo is None:
        raise CertificationInvalid(f"{label} has no timezone")
    return parsed.astimezone(timezone.utc)


def load_certifications(path: Optional[str] = None) -> dict:
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


def certified_states(
    certs: dict, asset: str, timeframe: str, classifier: str,
    now: Optional[datetime] = None,
) -> Optional[dict]:
    entry = certs.get(_cert_key(asset, timeframe, classifier))
    if not entry:
        return None
    exp = _parse_expiry(entry.get("expires_at", ""))
    if exp is not None:
        now = now or datetime.now(timezone.utc)
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
        if exp <= now:
            return None
    states = entry.get("states")
    return dict(states) if isinstance(states, dict) else {}


def backtest_classifier(regime_windows_spec: Optional[dict]) -> str:
    return "composite" if regime_windows_spec else "adx"


def _normalize_window_key(name: str) -> str:
    return (name or "").strip().lower()


def config_directional_classifier(regime_cfg: Optional[dict],
                                  sc: Optional[dict]) -> str:
    windows = (regime_cfg or {}).get("windows") or {}
    if not windows:
        return "adx"
    key = _normalize_window_key((sc or {}).get("regime_directional_window"))
    if key in ("", "default"):
        if "medium" in windows:
            key = "medium"
        else:
            names = sorted(_normalize_window_key(n) for n in windows)
            key = names[0] if names else ""
    for name, spec in windows.items():
        if _normalize_window_key(name) == key:
            c = str((spec or {}).get("classifier") or "").strip().lower()
            return c or "adx"
    return "adx"


def directional_cert_identity(strategy: Optional[dict], regime: Optional[dict]) -> Optional[dict]:
    """Mirror scheduler directionalCertIdentity.

    Returns None when the asset or timeframe cannot be resolved. The classifier
    is the directional window's classifier, matching regimeClassifierForWindow.
    """
    from regime import classifier_for_window, resolve_strategy_regime_window

    args = (strategy or {}).get("args") or []
    if len(args) < 3:
        return None
    symbol = str(args[1] or "").strip()
    timeframe = str(args[2] or "").strip()
    if (not symbol or not timeframe or symbol.startswith("-")
            or timeframe.startswith("-")):
        return None
    override = str((regime or {}).get("timeframe") or "").strip().lower()
    if override:
        timeframe = override
    window = resolve_strategy_regime_window(strategy or {}, "directional", regime or {})
    classifier = classifier_for_window(regime or {}, window)
    asset = normalize_cert_asset(symbol)
    if not asset or not timeframe:
        return None
    return {"asset": asset, "timeframe": timeframe, "classifier": classifier,
            "key": _cert_key(asset, timeframe, classifier)}
