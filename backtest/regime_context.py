"""One immutable regime context for a ledger comparison run.

Historical evidence and eligibility stay here. The Backtester receives only
the explicit feature columns and the certified states this module already
accepted. Nothing here talks to a live process or writes configuration.
"""

from __future__ import annotations

import hashlib
import json
import os
from datetime import datetime, timezone
from typing import Optional

from directional_certification import (
    CertificationInvalid,
    directional_cert_identity,
    parse_certifications_strict,
)
from regime import (
    RANGING_DIRECTIONAL_BARE,
    RANGING_DIRECTIONAL_SUBS,
    classifier_for_window,
    primary_regime_window_key,
    required_ohlcv_limit,
    resolve_strategy_regime_window,
)

REGIME_FIELDS = (
    "allowed_regimes", "regime_gate_on_failure", "regime_gate_window",
    "regime_directional_window", "regime_directional_policy",
    "regime_window_divergence", "regime_profile_allocation",
)

SIZING_REASON_CODES = {
    "capital_pct": (
        "sizing_capital_pct",
        "live derives capital again from the wallet balance in every cycle; "
        "the comparison keeps the verified starting cash",
    ),
}


def configuration_sha256(strategy: dict, regime: dict) -> str:
    body = json.dumps(
        {"regime": regime, "strategy": strategy},
        sort_keys=True, separators=(",", ":"), default=str,
    ).encode()
    return hashlib.sha256(body).hexdigest()


def _named(value) -> bool:
    return str(value or "").strip().lower() not in ("", "default")


def _windows(regime: dict) -> dict:
    windows = regime.get("windows") or {}
    return windows if isinstance(windows, dict) else {}


def _multi(regime: dict) -> bool:
    return bool(regime.get("enabled")) and bool(_windows(regime))


def _row(field, value, decision, reason, reason_code, approximable=False, present=None):
    return {
        "field": field,
        "category": "regime",
        "present": (field in () if present is None else present),
        "value": value,
        "active": decision not in ("inactive", "informational"),
        "decision": decision,
        "reason": reason,
        "reason_code": reason_code,
        "approximable": approximable,
    }


def _present_strategy(strategy, field):
    return field in strategy


def _fail_policy(strategy) -> str:
    raw = strategy.get("regime_gate_on_failure")
    if raw is None or raw == "":
        return "open"
    return str(raw).strip().lower()


PAYLOAD_ROW_TOKENS = {"unshifted_closed_candle": "closed_candle"}
PAYLOAD_ROW_NAMES = {"closed_candle": "unshifted_closed_candle", "decision_bar": "shifted_decision_bar"}


def _payload_timing(timing: dict, closed_bar: bool) -> dict:
    features = timing.get("features", {}) if timing.get("status") == "verified" else {}
    tokens = {}
    if "directional" in features:
        tokens["directional"] = features["directional"]
    if not closed_bar and "gate" in features:
        tokens["gate"] = features["gate"]
    values = sorted(set(tokens.values()))
    if not values:
        return {"status": "unattested", "row": "decision_bar", "tokens": tokens}
    if len(values) > 1:
        return {"status": "conflict", "row": None, "tokens": tokens}
    row = PAYLOAD_ROW_TOKENS.get(values[0])
    if row is None:
        return {"status": "unsupported", "row": None, "tokens": tokens}
    return {"status": "attested", "row": row, "tokens": tokens}


def _payload_block(payload: dict, *, needs_attestation: bool, consumer: str):
    status = payload["status"]
    if status == "attested":
        return None
    if status == "conflict":
        return ("refused", "regime_feature_timing_conflict", False,
                "verified timing evidence gives result.Regime more than one row "
                f"({', '.join(f'{k}={v}' for k, v in sorted(payload['tokens'].items()))}); "
                f"the {consumer} reads that one payload")
    if status == "unsupported":
        return ("refused", "regime_feature_timing_unsupported", True,
                "verified timing evidence names a result.Regime row the frozen candles do not reproduce; "
                "approximate mode uses the decision-bar label and records that substitute")
    if not needs_attestation:
        return None
    return ("refused", "regime_feature_timing_unsupported", True,
            f"the {consumer} reads result.Regime, and no verified timing evidence names its row; "
            "approximate mode uses the decision-bar label and records that substitute")


def _protection_source(stop_needs_labels, atr_named, protection_reads_gate, row_name, strategy, regime):
    if not stop_needs_labels or atr_named or not row_name:
        return None
    if protection_reads_gate:
        return row_name + ":gate_window:" + resolve_strategy_regime_window(strategy, "gate", regime)
    return row_name + ":primary_column"


def _direction_matches(entry: dict, cert_dir: str) -> bool:
    direction = str(entry.get("direction") or "").strip().lower()
    cert_dir = str(cert_dir or "").strip().lower()
    if not cert_dir:
        return False
    return direction == "both" or direction == cert_dir


def _honored(policy: dict, states: dict) -> dict:
    """Honor a policy entry the way live gatedDirectionalEntry does.

    An observed label is certified on its own key. Resolve then maps a
    ranging_directional_up or ranging_directional_down label onto a bare
    ranging_directional entry when the policy has no exact sub-label key.
    """
    trend = (policy or {}).get("trend_regime") or {}
    if not isinstance(trend, dict):
        return {}
    states = states or {}
    out = {}
    for label, entry in trend.items():
        if not isinstance(entry, dict):
            continue
        key = str(label)
        if _direction_matches(entry, states.get(key)):
            out[key] = dict(entry)
            continue
        if key != RANGING_DIRECTIONAL_BARE:
            continue
        for sub in sorted(RANGING_DIRECTIONAL_SUBS):
            if sub in trend:
                continue
            if _direction_matches(entry, states.get(sub)):
                out[key] = dict(entry)
                break
    return out


def _instant(text: str) -> Optional[datetime]:
    if not text:
        return None
    try:
        parsed = datetime.fromisoformat(str(text).replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.astimezone(timezone.utc)


def _binding_ok(entry: dict, binding: dict) -> bool:
    got = entry.get("binding")
    if not isinstance(got, dict):
        return False
    for key in ("partition", "strategy_id", "configuration_sha256"):
        if got.get(key) != binding.get(key):
            return False
    interval = got.get("interval")
    if not isinstance(interval, dict):
        return False
    start = _instant(interval.get("start"))
    end = _instant(interval.get("end"))
    want_start = _instant(binding.get("interval_start"))
    want_end = _instant(binding.get("interval_end"))
    if None in (start, end, want_start, want_end) or start != want_start or end != want_end:
        return False
    return entry.get("coverage") == "complete_interval"


def _sha256_file(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_artifact(base_dir: str, entry: dict, prefix: str) -> tuple:
    """Return (payload, refusal) for a bound evidence entry.

    refusal is a reason_code or None. payload is the parsed JSON object.
    """
    artifact = entry.get("artifact")
    if not isinstance(artifact, dict):
        return None, f"{prefix}_invalid"
    rel = artifact.get("path")
    want = str(artifact.get("sha256") or "")
    if not isinstance(rel, str) or not rel.strip() or not want:
        return None, f"{prefix}_invalid"
    path = rel if os.path.isabs(rel) else os.path.join(base_dir, rel)
    if not os.path.isfile(path):
        return None, f"{prefix}_missing"
    try:
        digest = _sha256_file(path)
    except OSError:
        return None, f"{prefix}_missing"
    if digest != want:
        return None, f"{prefix}_hash_mismatch"
    try:
        with open(path, "rb") as fh:
            text = fh.read().decode("utf-8")
        if prefix == "directional_certification":
            def _reject_constant(name):
                raise json.JSONDecodeError(f"unsupported JSON constant {name}", text, 0)
            payload = json.loads(text, parse_constant=_reject_constant)
        else:
            payload = json.loads(text)
    except (OSError, UnicodeError, json.JSONDecodeError):
        return None, f"{prefix}_invalid"
    if not isinstance(payload, dict):
        return None, f"{prefix}_invalid"
    return payload, None


def _cert_evidence(entry, binding, base_dir, identity, start, end) -> dict:
    """Resolve the certification set for the whole interval.

    status is one of verified, unverified, refused. A refused result carries
    reason_code. verified carries states (possibly empty) and constant=True.
    """
    if not isinstance(entry, dict) or not entry:
        return {"status": "unverified", "reason_code": "directional_certification_unverified",
                "approximable": True,
                "assumption": "no verified certification artifact; approximate mode assumes an empty set "
                              "and the base direction"}
    verified_shape = (entry.get("status") == "verified"
                      and isinstance(entry.get("evidence"), str) and entry["evidence"].strip()
                      and isinstance(entry.get("rule"), str) and entry["rule"].strip())
    if not verified_shape:
        return {"status": "unverified", "reason_code": "directional_certification_unverified",
                "approximable": True,
                "assumption": "certification evidence is not verified; approximate mode assumes an empty set"}
    if not _binding_ok(entry, binding):
        return {"status": "refused", "reason_code": "directional_certification_incomplete",
                "approximable": False,
                "detail": "certification evidence must bind the partition, strategy, configuration "
                          "and the complete comparison interval, including reloads"}
    if entry.get("reloads") != "none":
        return {"status": "refused", "reason_code": "directional_certification_changes_in_window",
                "approximable": False,
                "detail": "certification evidence records a reload or artifact change inside the interval; "
                          "open positions keep the states frozen at entry and flat decisions use the "
                          "current expiry, and this comparison does not model a change"}
    effective = entry.get("effective")
    artifact = entry.get("artifact")
    if (effective == "empty") == (artifact is not None):
        return {"status": "refused", "reason_code": "directional_certification_invalid",
                "approximable": False,
                "detail": "certification evidence needs exactly one of effective=empty or artifact"}
    if effective == "empty":
        table = {}
    else:
        payload, code = _read_artifact(base_dir, entry, "directional_certification")
        if code:
            return {"status": "refused", "reason_code": code, "approximable": False,
                    "detail": "the certification artifact failed to load"}
        try:
            table = parse_certifications_strict(payload)
        except CertificationInvalid as exc:
            return {"status": "refused", "reason_code": "directional_certification_invalid",
                    "approximable": False,
                    "detail": str(exc)}
    if identity is None:
        return {"status": "refused", "reason_code": "directional_certification_invalid",
                "approximable": False,
                "detail": "the strategy has no certification identity"}
    found = table.get(identity["key"])
    states = dict(found["states"]) if found else {}
    expires = _instant(found["expires_at"]) if found and found.get("expires_at") else None
    if expires is not None and start <= expires < end:
        return {"status": "refused", "reason_code": "directional_certification_changes_in_window",
                "approximable": False,
                "detail": "the certification expires inside the interval; flat decisions would use the "
                          "current expiry and an open position would keep the states frozen at entry"}
    if expires is not None and expires <= start:
        states = {}
    return {"status": "verified", "states": states, "identity": identity,
            "lifetime": "flat decisions check the current expiry; an open position uses the "
                        "states frozen at entry"}


def _label_evidence(entry, binding, base_dir) -> dict:
    if not isinstance(entry, dict) or not entry:
        return {"status": "unverified", "reason_code": "regime_label_availability_unverified",
                "approximable": True,
                "assumption": "no verified regime-label evidence; approximate mode assumes no bundle outage"}
    verified_shape = (entry.get("status") == "verified"
                      and isinstance(entry.get("evidence"), str) and entry["evidence"].strip()
                      and isinstance(entry.get("rule"), str) and entry["rule"].strip())
    if not verified_shape:
        return {"status": "unverified", "reason_code": "regime_label_availability_unverified",
                "approximable": True,
                "assumption": "regime-label evidence is not verified; approximate mode assumes no bundle outage"}
    if not _binding_ok(entry, binding):
        return {"status": "refused", "reason_code": "regime_labels_incomplete", "approximable": False,
                "detail": "label evidence must bind the partition, strategy, configuration and the "
                          "complete comparison interval"}
    payload, code = _read_artifact(base_dir, entry, "regime_labels")
    if code:
        return {"status": "refused", "reason_code": code, "approximable": False,
                "detail": "the regime-label evidence failed to load"}
    if payload.get("values_and_timing") is not True:
        return {"status": "refused", "reason_code": "regime_labels_incomplete", "approximable": False,
                "detail": "evidence that labels existed does not prove their values or timing matched "
                          "the frozen final candles"}
    return {"status": "verified"}


def _timing_evidence(entry, binding) -> dict:
    if not isinstance(entry, dict) or not entry:
        return {"status": "absent"}
    verified_shape = (entry.get("status") == "verified"
                      and isinstance(entry.get("evidence"), str) and entry["evidence"].strip()
                      and isinstance(entry.get("rule"), str) and entry["rule"].strip())
    if not verified_shape or not _binding_ok(entry, binding):
        return {"status": "absent"}
    features = entry.get("features")
    if not isinstance(features, dict):
        return {"status": "absent"}
    return {"status": "verified", "features": {str(k): str(v) for k, v in features.items()}}


def _selector_refusal(strategy, regime, field) -> Optional[dict]:
    raw_name = {"gate": "regime_gate_window", "directional": "regime_directional_window",
                "atr": "regime_atr_window"}[field]
    raw = strategy.get(raw_name)
    if not _named(raw):
        return None
    windows = _windows(regime)
    if not _multi(regime):
        return {"reason_code": "regime_window_multi_disabled", "approximable": False,
                "detail": f"{raw_name} names a window but regime.windows is not enabled"}
    key = str(raw).strip().lower()
    known = {str(name).strip().lower() for name in windows}
    if key not in known:
        return {"reason_code": "regime_window_unknown", "approximable": False,
                "detail": f"{raw_name} {raw!r} is not in regime.windows"}
    return None


def resolve_regime_context(segment: dict, evidence: dict, binding: dict, market: dict,
                           stop_needs_labels: bool, mode: str, base_dir: str) -> dict:
    strategy = segment.get("strategy") or {}
    regime = segment.get("regime") or {}
    evidence = evidence or {}
    windows = _windows(regime)
    enabled = bool(regime.get("enabled"))
    period = int(regime.get("period") or 14)
    adx = float(regime.get("adx_threshold") or 20.0)
    limit = required_ohlcv_limit(period, windows or None)
    warmup = market.get("warmup_bars")
    interval = str(market.get("interval") or "")
    timeframe = str(regime.get("timeframe") or "").strip().lower()
    closed_bar = strategy.get("closed_bar_decisions") is True
    allowed = strategy.get("allowed_regimes") or []
    if not isinstance(allowed, list):
        allowed = []
    fail = _fail_policy(strategy)
    start = _instant(binding.get("interval_start"))
    end = _instant(binding.get("interval_end"))
    identity = directional_cert_identity(strategy, regime)
    cert = _cert_evidence(evidence.get("directional_certification"), binding, base_dir,
                          identity, start, end)
    labels = _label_evidence(evidence.get("regime_labels"), binding, base_dir)
    timing = _timing_evidence(evidence.get("regime_feature_timing"), binding)
    payload = _payload_timing(timing, closed_bar)
    payload_attested = payload["status"] == "attested"

    rows = []
    blocking = []
    approximations = []

    def add(field, value, decision, code, approximable=False, reason=""):
        present = _present_strategy(strategy, field) if not str(field).startswith("regime.") else (
            str(field).split(".", 1)[1] in regime)
        rows.append(_row(field, value, decision, reason, code, approximable, present))
        if decision == "refused":
            item = {"field": field, "reason_code": code, "reason": reason,
                    "approximable": approximable, "category": "regime"}
            if approximable and mode == "approximate":
                approximations.append({
                    "feature": field, "category": "regime", "reason_code": code,
                    "assumption": reason, "effect": "simulated with the recorded substitute",
                })
            else:
                blocking.append(item)

    gate_selector = _selector_refusal(strategy, regime, "gate")
    dir_selector = _selector_refusal(strategy, regime, "directional")
    atr_named = _named(strategy.get("regime_atr_window"))
    # Live protectionATRRegimeLabel uses a named ATR window when one is set.
    # A default ATR selector reads the gate-window stamp on the position.
    protection_reads_gate = bool(
        stop_needs_labels and _named(strategy.get("regime_gate_window"))
        and not atr_named and gate_selector is None)
    stop_reads_payload = bool(stop_needs_labels and not atr_named)
    stop_block = (_payload_block(payload, needs_attestation=False, consumer="regime-owned stop")
                  if stop_reads_payload else None)
    stop_reads_columns = bool(stop_reads_payload and (protection_reads_gate or payload_attested))
    policy = strategy.get("regime_directional_policy")
    policy_set = isinstance(policy, dict) and len(policy) > 0
    honored = {}
    if policy_set and cert.get("status") == "verified":
        honored = _honored(policy, cert.get("states") or {})

    gate_active = enabled and len(allowed) > 0 and fail in ("open", "closed")
    if not enabled and len(allowed) > 0 and fail == "closed":
        gate_state = ("refused", "regime_gate_disabled_fail_closed", False,
                      "regime.enabled is false with allowed_regimes and a fail-closed policy; "
                      "the gate label is always empty, so the strategy could never open")
        gate_active = False
    elif not enabled and len(allowed) > 0 and fail == "open":
        gate_state = ("inactive", "regime_gate_inactive_open", False,
                      "regime.enabled is false and the open policy makes the gate a no-op")
        gate_active = False
    elif fail not in ("open", "closed") and len(allowed) > 0:
        gate_state = ("refused", "regime_gate_on_failure_invalid", False,
                      "regime_gate_on_failure must be open or closed")
        gate_active = False
    elif gate_selector and (gate_active or _named(strategy.get("regime_gate_window"))):
        gate_state = ("refused", gate_selector["reason_code"], False, gate_selector["detail"])
        gate_active = False
    elif not gate_active:
        gate_state = ("inactive", "regime_selector_no_consumer", False,
                      "no allowed_regimes consumer reads the gate")
    else:
        gate_state = None

    dir_active = policy_set and cert.get("status") == "verified" and bool(honored)
    if dir_selector and (_named(strategy.get("regime_directional_window")) or dir_active):
        dir_state = ("refused", dir_selector["reason_code"], False, dir_selector["detail"])
        dir_active = False
    elif not policy_set:
        dir_state = ("inactive", "regime_selector_no_consumer", False,
                     "no directional policy is configured")
    elif cert.get("status") == "unverified":
        dir_state = ("refused", cert["reason_code"], True, cert["assumption"])
        dir_active = False
    elif cert.get("status") == "refused":
        dir_state = ("refused", cert["reason_code"], cert.get("approximable", False),
                     cert.get("detail") or cert["reason_code"])
        dir_active = False
    elif not honored:
        dir_state = ("inactive", "directional_policy_uncertified", False,
                     "uncertified for the whole interval; live trades the base direction")
        dir_active = False
    else:
        dir_state = None

    consumers = []
    if gate_state is None:
        consumers.append("gate")
    if dir_state is None:
        consumers.append("directional")
    if stop_needs_labels:
        consumers.append("atr")
    if stop_reads_columns:
        consumers.append("protection")

    timeframe_blocks = bool(timeframe) and timeframe != interval.strip().lower() and bool(consumers)
    lookback_blocks = (warmup is None or int(warmup) < limit) and bool(
        [c for c in consumers if c in ("gate", "directional", "protection")])

    def _feature_block(feature: str):
        if timeframe_blocks and feature in consumers:
            return ("refused", "regime_timeframe_unprepared", False,
                    "regime labels from another timeframe are not prepared by the frozen comparison")
        if lookback_blocks and feature in ("gate", "directional"):
            return ("refused", "regime_lookback_insufficient", False,
                    f"warm-up before the first scored decision is shorter than the live lookback ({limit})")
        if feature == "gate" and not closed_bar:
            block = _payload_block(payload, needs_attestation=True, consumer="gate")
            if block:
                return block
        if feature == "directional":
            block = _payload_block(payload, needs_attestation=True, consumer="directional policy")
            if block:
                return block
        if feature in ("gate", "directional") and labels.get("status") != "verified":
            return ("refused", labels["reason_code"], labels.get("approximable", False),
                    labels.get("detail") or labels.get("assumption") or labels["reason_code"])
        return None

    if gate_state is None:
        gate_detail = (
            "the gate reads the shifted closed-bar label of its window" if closed_bar else
            "the gate reads the result.Regime label of its window on the unshifted closed-candle row")
        gate_state = _feature_block("gate") or (
            "modeled", "regime_gate_modeled", False, gate_detail)
    if dir_state is None:
        dir_state = _feature_block("directional") or (
            "modeled", "regime_directional_modeled", False,
            "the directional policy reads the result.Regime label of its window on the unshifted "
            "closed-candle row; an open position keeps the stamp from entry")

    payload_row = payload["row"] if payload_attested else "decision_bar"
    row_name = PAYLOAD_ROW_NAMES[payload_row] if payload["status"] != "conflict" else None

    gate_window_decision = gate_state
    if gate_selector and _named(strategy.get("regime_gate_window")):
        gate_window_decision = ("refused", gate_selector["reason_code"], False, gate_selector["detail"])
    elif protection_reads_gate and lookback_blocks:
        gate_window_decision = (
            "refused", "regime_lookback_insufficient", False,
            f"warm-up before the first scored decision is shorter than the live lookback ({limit})")
    elif protection_reads_gate and timeframe_blocks:
        gate_window_decision = (
            "refused", "regime_timeframe_unprepared", False,
            "regime labels from another timeframe are not prepared by the frozen comparison")
    elif protection_reads_gate and stop_block:
        gate_window_decision = stop_block
    elif protection_reads_gate and gate_state[0] != "modeled":
        gate_window_decision = (
            "modeled", "regime_gate_window_protection", False,
            f"a regime-owned stop reads the gate-window label of result.Regime on the {row_name} row")
    elif gate_state[0] == "inactive":
        gate_window_decision = ("inactive", "regime_selector_no_consumer", False,
                                "selector with no active consumer")
    elif gate_state[0] == "modeled":
        gate_window_decision = ("modeled", "regime_gate_modeled", False, "gate window selector")

    if dir_selector and _named(strategy.get("regime_directional_window")):
        dir_window_decision = ("refused", dir_selector["reason_code"], False, dir_selector["detail"])
    elif dir_state[0] == "inactive":
        dir_window_decision = ("inactive", "regime_selector_no_consumer", False,
                               "selector with no active consumer")
    elif dir_state[0] == "modeled":
        dir_window_decision = ("modeled", "regime_directional_modeled", False, "directional window selector")
    else:
        dir_window_decision = dir_state

    fail_decision = gate_state if strategy.get("regime_gate_on_failure") is not None or gate_state[0] != "inactive" else (
        "inactive", "regime_selector_no_consumer", False, "not configured")
    if gate_state[0] == "inactive" and not _named(strategy.get("regime_gate_window")):
        if strategy.get("regime_gate_on_failure") is None and not allowed:
            fail_decision = ("inactive", "regime_selector_no_consumer", False, "not configured")

    atr_selector = _selector_refusal(strategy, regime, "atr")
    if atr_selector and _named(strategy.get("regime_atr_window")):
        atr_decision = ("refused", atr_selector["reason_code"], False, atr_selector["detail"])
    elif _named(strategy.get("regime_atr_window")):
        atr_decision = ("refused", "regime_atr_window_unsupported", False,
                        "a named ATR window stays refused; issue 1732 owns that constructor input")
    else:
        atr_decision = ("inactive", "regime_selector_no_consumer", False,
                        "selector with no active consumer")

    add("allowed_regimes", strategy.get("allowed_regimes"), *gate_state)
    add("regime_gate_on_failure", strategy.get("regime_gate_on_failure"), *fail_decision)
    add("regime_gate_window", strategy.get("regime_gate_window"), *gate_window_decision)
    add("regime_atr_window", strategy.get("regime_atr_window"), *atr_decision)
    add("regime_directional_window", strategy.get("regime_directional_window"), *dir_window_decision)
    add("regime_directional_policy", policy, *dir_state)

    div = strategy.get("regime_window_divergence")
    div_configured = (
        (isinstance(div, dict) and len(div) > 0)
        or (not isinstance(div, dict) and div not in (None, False, "", 0))
    )
    if div_configured:
        add("regime_window_divergence", div, "refused", "regime_divergence_unmodeled", False,
            "the Backtester has no model for regime window divergence")
    else:
        add("regime_window_divergence", div, "inactive", "regime_selector_no_consumer", False,
            "not configured")

    profile = strategy.get("regime_profile_allocation")
    if isinstance(profile, dict) and len(profile) > 0:
        add("regime_profile_allocation", profile, "refused", "regime_profile_allocation_unsupported", False,
            "the execution-spec path rejects profile allocation")
    else:
        add("regime_profile_allocation", profile, "inactive", "regime_selector_no_consumer", False,
            "not configured")

    modeled = [r for r in rows if r["decision"] == "modeled"]
    refused_consumers = [r for r in rows if r["decision"] == "refused" and r["field"] in (
        "allowed_regimes", "regime_directional_policy", "regime_window_divergence",
        "regime_profile_allocation")]
    if not enabled:
        add("regime.enabled", regime.get("enabled"), "inactive", "regime_enabled_inactive", False,
            "regime disabled")
    elif refused_consumers and not any(r["field"] in ("allowed_regimes", "regime_directional_policy")
                                       and r["decision"] == "modeled" for r in rows) and not stop_needs_labels:
        sample = refused_consumers[0]
        add("regime.enabled", True, "refused", sample["reason_code"], sample["approximable"],
            f"an unmodeled regime consumer is active ({sample['field']})")
    elif stop_block:
        add("regime.enabled", True, *stop_block)
    elif stop_reads_columns and lookback_blocks:
        add("regime.enabled", True, "refused", "regime_lookback_insufficient", False,
            f"the regime-owned stop reads the bounded result.Regime label, and the warm-up before the "
            f"first scored decision is shorter than the live lookback ({limit})")
    elif modeled or stop_needs_labels:
        add("regime.enabled", True, "modeled", "regime_enabled_modeled", False,
            "a modeled consumer reads regime labels")
    else:
        add("regime.enabled", True, "informational", "regime_enabled_informational", False,
            "no consumer reads labels; labels would only stamp trades")

    if timeframe:
        if timeframe_blocks:
            add("regime.timeframe", regime.get("timeframe"), "refused", "regime_timeframe_unprepared", False,
                "regime labels from another timeframe are not prepared by the frozen comparison")
        else:
            add("regime.timeframe", regime.get("timeframe"), "informational", "regime_timeframe_matches", False,
                "labels come from the frozen dataset interval")

    def _usable(field: str) -> bool:
        field_rows = [r for r in rows if r["field"] == field]
        if any(r["decision"] == "modeled" for r in field_rows):
            return True
        return (mode == "approximate"
                and any(r["decision"] == "refused" and r["approximable"] for r in field_rows)
                and not any(r["decision"] == "refused" and not r["approximable"] for r in field_rows))

    use_gate = _usable("allowed_regimes")
    use_dir = _usable("regime_directional_policy")
    use_stop_columns = bool(
        stop_reads_columns and not lookback_blocks and not timeframe_blocks
        and (stop_block is None or (mode == "approximate" and stop_block[2])))

    prepare = None
    engine = None
    if (use_gate or use_dir or use_stop_columns) and not lookback_blocks and not timeframe_blocks:
        gate_key = resolve_strategy_regime_window(strategy, "gate", regime)
        dir_key = resolve_strategy_regime_window(strategy, "directional", regime)
        columns = {}
        primary = primary_regime_window_key(windows) if windows else ""

        def col(key: str) -> str:
            slug = (key or "primary").strip().lower() or "primary"
            return "regime_w_" + "".join(ch if ch.isalnum() else "_" for ch in slug)

        columns[col(primary or "primary")] = primary
        columns[col(gate_key)] = gate_key
        columns[col(dir_key)] = dir_key
        prepare = {
            "period": period,
            "adx_threshold": adx,
            "windows_spec": windows or None,
            "limit": limit,
            "columns": columns,
        }
        label_columns = {
            "gate": col(gate_key),
            "directional": col(dir_key),
            "directional_named": _named(strategy.get("regime_directional_window")),
            "payload_row": payload_row,
            "gate_row": "decision_bar" if closed_bar else "payload",
        }
        engine = {
            "regime_enabled": True,
            "regime_period": period,
            "regime_adx_threshold": adx,
            "regime_label_columns": label_columns,
        }
        if windows:
            engine["regime_windows_spec"] = windows
        if use_gate:
            engine["allowed_regimes"] = list(allowed)
            engine["regime_gate_on_failure"] = fail if fail in ("open", "closed") else "open"
        if use_dir and policy_set:
            engine["regime_directional_policy"] = policy
            engine["regime_directional_certified_states"] = dict(honored and cert.get("states") or {})
        elif policy_set and cert.get("status") == "verified" and not honored:
            engine["regime_directional_policy"] = policy
            engine["regime_directional_certified_states"] = {}

    protection_row = row_name if stop_reads_payload else None
    window_report = []
    for name in sorted(windows):
        spec = windows[name] if isinstance(windows[name], dict) else {"period": windows[name]}
        window_report.append({
            "window": name,
            "classifier": classifier_for_window(regime, name),
            "period": int(spec.get("period") or period) if isinstance(spec, dict) else int(spec),
        })
    report = {
        "lookback": limit,
        "warmup_bars": warmup,
        "windows": window_report,
        "consumers": {
            "gate": next(r["decision"] for r in rows if r["field"] == "allowed_regimes"),
            "directional": next(r["decision"] for r in rows if r["field"] == "regime_directional_policy"),
            "atr": (protection_row or "refused") if stop_needs_labels else "inactive",
        },
        "timing": {
            "gate": ("shifted_closed_bar" if closed_bar else
                     "unshifted_closed_candle" if payload_attested else
                     "unsupported_without_evidence"),
            "directional": "result.Regime",
            "protection": protection_row,
            "result_regime": {
                "status": payload["status"],
                "row": row_name,
                "tokens": dict(sorted(payload["tokens"].items())),
                "basis": "verified regime_feature_timing tokens that describe result.Regime "
                         "(features.directional, and features.gate with closed_bar_decisions off); "
                         "without one the row is the decision bar",
            },
        },
        "stamps": "Live writes one result.Regime payload per cycle. The position stamps, the regime-owned "
                  "stop, the regime-tier label and the flat directional decision read its row: the "
                  "unshifted closed-candle row when verified timing evidence attests it, otherwise the "
                  "decision bar. The gate reads that row with closed_bar_decisions off and the shifted "
                  "closed-bar label with it on. An open position keeps the gate-window label when the "
                  "directional selector is default, and the named window when it is set. A flat "
                  "directional decision reads the resolved directional window.",
        "certification": {
            "status": cert.get("status"),
            "identity": identity,
            "lifetime": cert.get("lifetime") or (
                "flat decisions check the current expiry; an open position uses the states frozen at entry"),
            "states": cert.get("states") or {},
        },
        "protection_source": _protection_source(
            stop_needs_labels, atr_named, protection_reads_gate, protection_row, strategy, regime),
        "label_columns": sorted((prepare or {}).get("columns") or {}),
    }
    return {
        "rows": rows,
        "engine": engine,
        "prepare": prepare,
        "report": report,
        "blocking": blocking,
        "approximations": approximations,
    }
