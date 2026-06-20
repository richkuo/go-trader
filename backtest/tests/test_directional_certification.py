"""#1085: backtest-side directional certification loader/verdict (parity with
scheduler/regime_directional_certification.go)."""
import json
from datetime import datetime, timedelta, timezone

from directional_certification import (
    normalize_cert_asset,
    load_certifications,
    is_directional_certified,
    backtest_classifier,
    config_directional_classifier,
)


def test_normalize_cert_asset():
    assert normalize_cert_asset("BTC/USDT") == "BTC"
    assert normalize_cert_asset("btc") == "BTC"
    assert normalize_cert_asset("BTC-PERP") == "BTC"
    assert normalize_cert_asset("ETH/USD") == "ETH"
    assert normalize_cert_asset("SOL_USDT") == "SOL"
    assert normalize_cert_asset("  xrp ") == "XRP"
    assert normalize_cert_asset("") == ""


def test_load_missing_is_failclosed_empty(tmp_path):
    assert load_certifications(str(tmp_path / "nope.json")) == {}


def test_load_malformed_is_failclosed_empty(tmp_path):
    p = tmp_path / "cert.json"
    p.write_text("{not json")
    assert load_certifications(str(p)) == {}
    p.write_text(json.dumps({"schema_version": 2, "certified": []}))
    assert load_certifications(str(p)) == {}


def test_is_directional_certified_active_expired_never(tmp_path):
    now = datetime(2026, 6, 19, tzinfo=timezone.utc)
    future = (now + timedelta(days=2)).isoformat().replace("+00:00", "Z")
    past = (now - timedelta(days=2)).isoformat().replace("+00:00", "Z")
    p = tmp_path / "cert.json"
    p.write_text(json.dumps({
        "schema_version": 1,
        "certified": [
            {"asset": "BTC/USDT", "timeframe": "1h", "classifier": "composite",
             "expires_at": future, "states": {"trending_up": "long"}},
            {"asset": "ETH", "timeframe": "4h", "classifier": "adx",
             "expires_at": past, "states": {"trending_down": "short"}},
        ],
    }))
    certs = load_certifications(str(p))
    # Live HL args use "BTC"; artifact "BTC/USDT" — must reconcile.
    assert is_directional_certified(certs, "BTC", "1h", "composite", now)
    # expired
    assert not is_directional_certified(certs, "ETH", "4h", "adx", now)
    # never
    assert not is_directional_certified(certs, "SOL", "1h", "composite", now)
    # classifier / timeframe mismatch
    assert not is_directional_certified(certs, "BTC", "1h", "adx", now)
    assert not is_directional_certified(certs, "BTC", "4h", "composite", now)


def test_backtest_classifier():
    assert backtest_classifier(None) == "adx"
    assert backtest_classifier({"windows": {}}) == "composite"


# Review finding 3: the backtest cert key must use the LIVE directional-window
# classifier, not "composite if any windows spec". Otherwise a config whose
# directional window is ADX but which carries a windows spec keys live on
# (asset,tf,adx) and backtest on (asset,tf,composite) → parity hole.
def test_config_directional_classifier_matches_live_resolution():
    # No regime.windows → legacy single-lookback ADX.
    assert config_directional_classifier({}, {}) == "adx"
    assert config_directional_classifier({"windows": {}}, {}) == "adx"

    windows = {
        "short": {"classifier": "adx", "period": 14},
        "medium": {"classifier": "composite", "period": 48},
    }
    rc = {"enabled": True, "windows": windows}

    # Directional window names an ADX window (the divergence case) → adx, even
    # though a windows spec is present (backtest_classifier would wrongly say
    # composite here).
    sc_adx = {"regime_directional_window": "short"}
    assert config_directional_classifier(rc, sc_adx) == "adx"
    assert backtest_classifier(windows) == "composite"  # the bug this fixes

    # Names a composite window → composite.
    sc_comp = {"regime_directional_window": "medium"}
    assert config_directional_classifier(rc, sc_comp) == "composite"

    # Unset/"default" → primary window: "medium" preferred when present.
    assert config_directional_classifier(rc, {}) == "composite"
    assert config_directional_classifier(rc, {"regime_directional_window": "default"}) == "composite"

    # Unset, no "medium" → first sorted window name.
    rc2 = {"windows": {"b_win": {"classifier": "composite"}, "a_win": {"classifier": "adx"}}}
    assert config_directional_classifier(rc2, {}) == "adx"  # a_win sorts first

    # A window with a blank classifier defaults to adx (effectiveClassifier).
    rc3 = {"windows": {"medium": {"period": 48}}}
    assert config_directional_classifier(rc3, {}) == "adx"


def test_repo_artifact_is_empty_and_valid():
    """The committed artifact must parse and certify nothing (#1076)."""
    import os
    here = os.path.dirname(os.path.abspath(__file__))
    artifact = os.path.join(here, "..", "research",
                            "regime_directional_certifications.json")
    certs = load_certifications(artifact)
    assert certs == {}, "the shipped artifact must certify nothing (#1076)"
