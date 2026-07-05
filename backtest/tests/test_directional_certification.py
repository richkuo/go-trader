"""#1085: backtest-side directional certification loader/verdict (parity with
scheduler/regime_directional_certification.go)."""
import json
from datetime import datetime, timedelta, timezone

from directional_certification import (
    normalize_cert_asset,
    load_certifications,
    is_directional_certified,
    certified_states,
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


def test_certified_states_active_expired_never(tmp_path):
    # #1085 per-state: certified_states returns the per-state direction MAP for an
    # active cell, None for expired/never (fail-closed → base direction).
    now = datetime(2026, 6, 19, tzinfo=timezone.utc)
    future = (now + timedelta(days=2)).isoformat().replace("+00:00", "Z")
    past = (now - timedelta(days=2)).isoformat().replace("+00:00", "Z")
    p = tmp_path / "cert.json"
    p.write_text(json.dumps({
        "schema_version": 1,
        "certified": [
            {"asset": "BTC/USDT", "timeframe": "1h", "classifier": "composite",
             "expires_at": future,
             "states": {"trending_up": "long", "trending_down": "short"}},
            {"asset": "ETH", "timeframe": "4h", "classifier": "adx",
             "expires_at": past, "states": {"trending_down": "short"}},
        ],
    }))
    certs = load_certifications(str(p))
    assert certified_states(certs, "BTC", "1h", "composite", now) == {
        "trending_up": "long", "trending_down": "short"}
    assert certified_states(certs, "ETH", "4h", "adx", now) is None      # expired
    assert certified_states(certs, "SOL", "1h", "composite", now) is None  # never


def test_gate_directional_policy_by_states_per_state_sign():
    # #1085 review-finding fix (parity with Go gatedDirectionalEntry): the gate is
    # PER STATE — a state whose configured side contradicts the certified sign (or
    # is uncertified) is dropped → base; "both" never contradicts; None = honor-all.
    from backtester import _gate_directional_policy_by_states
    policy = {
        "trending_up": {"direction": "short", "invert_signal": True},  # operator wants short
        "trending_down": {"direction": "short"},
        "ranging": {"direction": "long"},
    }
    certs = {"trending_up": "long", "trending_down": "short", "ranging": "long"}
    gated = _gate_directional_policy_by_states(policy, certs)
    assert "trending_up" not in gated                       # (a) sign contradiction → base
    assert gated["trending_down"]["direction"] == "short"   # (b) match stays honored
    assert gated["ranging"]["direction"] == "long"          # (b) match stays honored
    # (c) config "both" never contradicts → kept.
    pol_both = {"trending_up": {"direction": "both"}}
    assert _gate_directional_policy_by_states(pol_both, {"trending_up": "long"}) == pol_both
    # Absent (uncertified) state → dropped; only the certified-and-matching one stays.
    assert _gate_directional_policy_by_states(policy, {"trending_up": "short"}) == {
        "trending_up": {"direction": "short", "invert_signal": True}}
    # Uncertified cell (empty map) → everything dropped.
    assert _gate_directional_policy_by_states(policy, {}) == {}
    # None → legacy cell-level honor-all (policy returned unchanged).
    assert _gate_directional_policy_by_states(policy, None) == policy


def test_repo_artifact_is_empty_and_valid():
    """The committed artifact must parse and certify nothing (#1076)."""
    import os
    here = os.path.dirname(os.path.abspath(__file__))
    artifact = os.path.join(here, "..", "research",
                            "regime_directional_certifications.json")
    certs = load_certifications(artifact)
    assert certs == {}, "the shipped artifact must certify nothing (#1076)"


def test_gate_expands_bare_policy_onto_certified_subs():
    # #1124/#1228: live Resolve falls back from a sub-label stamp to the bare
    # ranging_directional policy entry. A cert for ranging_directional_up must
    # therefore honor a bare-only policy the way live does.
    from backtester import _gate_directional_policy_by_states
    bare_only = {"ranging_directional": {"direction": "long"}}
    gated = _gate_directional_policy_by_states(
        bare_only, {"ranging_directional_up": "long"})
    assert gated == {"ranging_directional_up": {"direction": "long"}}
    # Contradicting cert still drops the expanded entry.
    assert _gate_directional_policy_by_states(
        bare_only, {"ranging_directional_up": "short"}) == {}
    # An explicit sub key wins over the bare expansion (exact match first).
    mixed = {
        "ranging_directional": {"direction": "long"},
        "ranging_directional_up": {"direction": "both"},
    }
    gated = _gate_directional_policy_by_states(
        mixed, {"ranging_directional_up": "short"})
    assert gated == {"ranging_directional_up": {"direction": "both"}}


def test_resolve_directional_entry_is_exact_match_only():
    # #1228 review round 2: runtime resolution runs against the ALREADY-GATED
    # policy, whose gate expands bare->subs subject to each sub's own cert. A
    # resolve-level bare fallback would resurrect the override for a sub the
    # gate dropped as uncertified — diverging from live's fail-closed
    # gatedDirectionalEntry. So resolution is exact-match only.
    from backtester import _resolve_regime_directional_entry
    bare_only = {"ranging_directional": {"direction": "short"}}
    assert _resolve_regime_directional_entry(
        bare_only, "ranging_directional_down") is None
    assert _resolve_regime_directional_entry(bare_only, "trending_up") is None
    assert _resolve_regime_directional_entry(
        bare_only, "ranging_directional") == {"direction": "short"}


def test_gate_then_resolve_matches_live_cert_semantics():
    # End-to-end gate+resolve composition, the three review must-survive cases.
    from backtester import (
        _gate_directional_policy_by_states,
        _resolve_regime_directional_entry,
    )
    bare_policy = {"ranging_directional": {"direction": "long"}}
    # (1) bare policy + bare-only cert + stamped _down -> BASE (live
    # fails-closed: cert lookup is exact on the stamped sub).
    gated = _gate_directional_policy_by_states(
        bare_policy, {"ranging_directional": "long"})
    assert _resolve_regime_directional_entry(
        gated, "ranging_directional_down") is None
    # ...while a bare stamp still honors the bare override.
    assert _resolve_regime_directional_entry(
        gated, "ranging_directional") == {"direction": "long"}
    # (2) bare policy + sub cert + stamped _up -> bare override honored via
    # the gate's cert-aware expansion.
    gated = _gate_directional_policy_by_states(
        bare_policy, {"ranging_directional_up": "long"})
    assert _resolve_regime_directional_entry(
        gated, "ranging_directional_up") == {"direction": "long"}
    # (3) bare direction contradicts the sub's certified sign -> base.
    gated = _gate_directional_policy_by_states(
        bare_policy, {"ranging_directional_up": "short"})
    assert _resolve_regime_directional_entry(
        gated, "ranging_directional_up") is None
