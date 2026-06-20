"""#1085: the certification producer's pure gate (regime_1076_certify.certify).

Tests the global-correction + sign-alignment + held-out-forward gate over
synthetic premise-screen rows — no data access."""
import os
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import regime_1076_certify as certify_mod  # noqa: E402

GEN_AT = datetime(2026, 6, 19, tzinfo=timezone.utc)


def _row(symbol="BTC/USDT", timeframe="1h", classifier="composite",
         state="trending_up", window="oos", p=0.0001, policy_dir=1,
         sign_aligned=True):
    return {
        "classifier": classifier, "symbol": symbol, "timeframe": timeframe,
        "window": window, "horizon": 4, "state": state, "gap": 0.01,
        "mean_fwd": 0.01, "p_value": p, "fdr_reject": True,
        "policy_dir": policy_dir, "sign_aligned": sign_aligned,
        "candidate_edge": True,
    }


def test_empty_rows_certify_nothing():
    art = certify_mod.certify([], generated_at=GEN_AT)
    assert art["schema_version"] == 1
    assert art["certified"] == []


def test_surviving_aligned_heldout_row_is_certified():
    art = certify_mod.certify([_row()], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    e = art["certified"][0]
    assert (e["asset"], e["timeframe"], e["classifier"]) == ("BTC", "1h", "composite")
    assert e["states"] == {"trending_up": "long"}
    assert e["expires_at"] > e["generated_at"]


def test_historical_window_not_certified():
    # Same strong row but in a historical window → not held-out-forward.
    art = certify_mod.certify([_row(window="2024")], generated_at=GEN_AT)
    assert art["certified"] == []


def test_wrong_signed_not_certified():
    art = certify_mod.certify([_row(sign_aligned=False)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_non_significant_not_certified():
    # A large p-value should not survive global BH even alone.
    art = certify_mod.certify([_row(p=0.9)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_composite_sublabel_maps_to_canonical():
    art = certify_mod.certify(
        [_row(state="trending_down_strong", policy_dir=-1)], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    assert art["certified"][0]["states"] == {"trending_down": "short"}
