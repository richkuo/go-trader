import time
from typing import Any, Dict, Optional


def basket_scripted_fixture_core(frames: Dict[str, Any], eligibility: Dict[str, Any],
                                 weights: Optional[dict] = None, scores: Optional[dict] = None,
                                 hold: str = "", sleep_seconds: float = 0, raw: Any = None) -> Any:
    if sleep_seconds:
        time.sleep(float(sleep_seconds))
    if raw is not None:
        return raw
    if hold:
        return {"hold": str(hold)}
    eligible = sorted(sym for sym, rec in (eligibility or {}).items() if rec.get("status") == "eligible")
    weights = dict(weights or {})
    scores = dict(scores or {})
    out_weights = {sym: float(weights.get(sym, 0.0)) for sym in eligible}
    out_scores = {sym: float(scores.get(sym, out_weights[sym])) for sym in eligible}
    return {"scores": out_scores, "weights": out_weights, "exclusions": {}}
