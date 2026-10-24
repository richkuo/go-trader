import importlib.util
import math
import os
import sys
from fractions import Fraction

POLICY = "cross_exchange_perp_arbitrage"
REQUEST_VERSION = 2
RESULT_VERSION = 2
PARAM_KEYS = ("adverse_band_bps", "entry_threshold_bps", "exit_threshold_bps", "funding_allowance_bps")
BPS = Fraction(10000)
MAX_THRESHOLD_BPS = Fraction(10000)
PROVIDER_HYPERLIQUID = "hyperliquid"
PROVIDER_OKX = "okx"
ORIENTATIONS = {
    (PROVIDER_HYPERLIQUID, PROVIDER_OKX): "long_hyperliquid_short_okx",
    (PROVIDER_OKX, PROVIDER_HYPERLIQUID): "long_okx_short_hyperliquid",
}
VENUE_CAP_FIELDS = {PROVIDER_HYPERLIQUID: "hyperliquid_notional_cap_usd", PROVIDER_OKX: "okx_notional_cap_usd"}
LIMIT_FIELDS = ("gross_budget_usd", "hyperliquid_notional_cap_usd", "okx_notional_cap_usd", "unmatched_cap_usd")
LIFECYCLE_FLAT = "flat"
LIFECYCLE_MATCHED = "matched"
MAX_SUMMARIES = 16
MAX_REASON_BYTES = 1024

_TOOLS = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))), "shared_tools")


def _load_tool(name):
    path = os.path.join(_TOOLS, name + ".py")
    loaded = sys.modules.get(name)
    if loaded is not None:
        origin = getattr(loaded, "__file__", None) or ""
        if os.path.realpath(origin) != os.path.realpath(path):
            raise ImportError(f"module {name} is loaded from {origin!r}, not the repository helper {path!r}")
        return loaded
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


pair_evidence = _load_tool("pair_evidence")
pair_inputs = _load_tool("pair_inputs")

JSONNumber = pair_evidence.JSONNumber
PairEvidenceError = pair_evidence.PairEvidenceError
_KEY_NAMES = tuple(name for name, _ in pair_evidence.KEY_FIELDS)


class PolicyInvalid(Exception):
    def __init__(self, stage, reason):
        super().__init__(reason)
        self.stage = stage
        self.reason = reason


class PolicyHold(Exception):
    def __init__(self, stage, reason):
        super().__init__(reason)
        self.stage = stage
        self.reason = reason


def _kind(node):
    if node is None:
        return "null"
    if isinstance(node, bool):
        return "boolean"
    if isinstance(node, str):
        return "string"
    if isinstance(node, (JSONNumber, int)):
        return "number"
    if isinstance(node, list):
        return "array"
    if isinstance(node, dict):
        return "object"
    return type(node).__name__


def _number_text(node):
    if isinstance(node, bool) or not isinstance(node, (JSONNumber, int)):
        return None
    text = node.text if isinstance(node, JSONNumber) else str(node)
    try:
        if not math.isfinite(float(text)):
            return None
    except (ValueError, OverflowError):
        return None
    return text


def _number(node, path, stage="request"):
    text = _number_text(node)
    if text is None:
        raise PolicyInvalid(stage, f"{path} must be a finite JSON number, got {_kind(node)}")
    return Fraction(text)


def _string(node, path, stage="request"):
    if not isinstance(node, str):
        raise PolicyInvalid(stage, f"{path} must be a JSON string, got {_kind(node)}")
    return node


def _object(node, path, stage="request"):
    if not isinstance(node, dict):
        raise PolicyInvalid(stage, f"{path} must be a JSON object, got {_kind(node)}")
    return node


def _x(value):
    try:
        return pair_evidence.fraction_decimal(value)
    except PairEvidenceError:
        return f"{value.numerator}/{value.denominator}"


def _clip(text):
    raw = text.encode("utf-8")
    if len(raw) <= MAX_REASON_BYTES:
        return text
    return raw[:MAX_REASON_BYTES].decode("utf-8", "ignore")


def param_errors(params):
    if not isinstance(params, dict):
        return [f"params must be a JSON object, got {_kind(params)}"], {}
    errs = []
    vals = {}
    for k in PARAM_KEYS:
        if k not in params:
            errs.append(f"params.{k} is missing; policy {POLICY} has no parameter defaults")
            continue
        text = _number_text(params[k])
        if text is None:
            errs.append(f"params.{k} must be a finite JSON number, got {_kind(params[k])}")
            continue
        vals[k] = Fraction(text)
    for k in sorted(set(params) - set(PARAM_KEYS)):
        errs.append(f"params has unknown field {k!r} for policy {POLICY}")
    entry = vals.get("entry_threshold_bps")
    exit_ = vals.get("exit_threshold_bps")
    if exit_ is not None and exit_ < 0:
        errs.append(f"params.exit_threshold_bps {_x(exit_)} must be at least 0")
    if entry is not None and entry > MAX_THRESHOLD_BPS:
        errs.append(f"params.entry_threshold_bps {_x(entry)} must not exceed {_x(MAX_THRESHOLD_BPS)}")
    if entry is not None and exit_ is not None and not exit_ < entry:
        errs.append(f"params.exit_threshold_bps {_x(exit_)} must be below entry_threshold_bps {_x(entry)}")
    adverse = vals.get("adverse_band_bps")
    if adverse is not None and not (0 <= adverse < MAX_THRESHOLD_BPS):
        errs.append(f"params.adverse_band_bps {_x(adverse)} must be at least 0 and below {_x(MAX_THRESHOLD_BPS)}")
    funding = vals.get("funding_allowance_bps")
    if funding is not None and funding < 0:
        errs.append(f"params.funding_allowance_bps {_x(funding)} must be at least 0")
    return sorted(errs), vals


def _identity(request):
    out = {"v": RESULT_VERSION, "pair_id": "", "slot_ms": 0, "contract_fingerprint": "", "input_fingerprint": ""}
    if not isinstance(request, dict):
        return out
    for k in ("pair_id", "contract_fingerprint", "input_fingerprint"):
        if isinstance(request.get(k), str):
            out[k] = request[k]
    slot = request.get("slot_ms")
    if isinstance(slot, JSONNumber):
        try:
            value = int(slot.text)
        except ValueError:
            value = None
        if value is not None and str(value) == slot.text and 0 <= value < (1 << 63):
            out["slot_ms"] = value
    return out


def _admit(request, decision):
    if not isinstance(request, dict):
        raise PolicyInvalid("request", f"the policy request must be a JSON object, got {_kind(request)}")
    version = request.get("v")
    if not (isinstance(version, JSONNumber) and version.text == str(REQUEST_VERSION)):
        shown = version.text if isinstance(version, JSONNumber) else _kind(version)
        raise PolicyInvalid("request", f"request version {shown} is not {REQUEST_VERSION}")
    if request.get("policy") != POLICY:
        raise PolicyInvalid("request", f"policy {request.get('policy')!r} is not {POLICY}")
    errs, vals = param_errors(request.get("params"))
    if errs:
        decision["param_errors"] = errs
        raise PolicyInvalid("params", "invalid parameters: " + "; ".join(errs))
    decision["params"] = {k: _x(vals[k]) for k in PARAM_KEYS}
    return vals


def _verify(request, decision):
    try:
        env = pair_inputs.verify_envelope(request)
    except (ValueError, KeyError, TypeError, AttributeError) as exc:
        raise PolicyInvalid("evidence", f"the saved quote envelope fails verification: {type(exc).__name__}: {exc}")
    decision["input_fingerprint"] = request["input_fingerprint"]
    decision["evidence"] = {
        m["key"]["member_id"]: {
            "capability_hash": m["capability"]["evidence_hash"],
            "account_hash": m["account"]["evidence_hash"],
            "conversion_hashes": [c["evidence_hash"] for c in m["conversions"]],
            "payload_hash": m["observation"]["payload_hash"],
            "first_receipt_ms": m["observation"]["first_receipt_ms"],
        }
        for m in env["members"]
    }
    return env


def _levels(levels):
    return [(Fraction(lv["px"]), Fraction(lv["base_qty"])) for lv in levels]


def _member(raw, plain, em, adverse, path):
    _object(raw, path)
    meta = _object(raw.get("meta"), path + ".meta")
    cost = _object(raw.get("cost_policy"), path + ".cost_policy")
    fee = _number(cost.get("taker_fee_pct"), path + ".cost_policy.taker_fee_pct")
    slip = _number(cost.get("slippage_pct"), path + ".cost_policy.slippage_pct")
    if not (0 <= fee < 1) or not (0 <= slip < 1):
        raise PolicyInvalid("request", f"{path}.cost_policy rates must be fractions in [0, 1)")
    leverage = _number(raw.get("leverage"), path + ".leverage")
    imr = _number(meta.get("initial_margin_rate"), path + ".meta.initial_margin_rate")
    mmr = _number(meta.get("maintenance_margin_rate"), path + ".meta.maintenance_margin_rate")
    if leverage <= 0 or not (0 < imr <= 1) or not (0 < mmr <= 1):
        raise PolicyInvalid("request", f"{path} needs a positive leverage and margin rates in (0, 1]")
    booked = _number(raw.get("booked_base_qty"), path + ".booked_base_qty")
    avg = _number(raw.get("booked_avg_px"), path + ".booked_avg_px")
    cap = em["capability"]
    cmeta = cap["meta"]
    contract_size = pair_evidence.decimal_fraction(cmeta["contract_size"], "contract_size")
    lot_native = pair_evidence.decimal_fraction(cmeta["lot_step"], "lot_step")
    rates = {c["currency"]: pair_evidence.decimal_fraction(c["rate_usd"], "rate_usd") for c in em["conversions"]}
    rates[pair_evidence.USD] = Fraction(1)
    for cur in (cmeta["price_currency"], cmeta["settlement_currency"]):
        if cur not in rates or rates[cur] <= 0:
            raise PolicyInvalid("evidence", f"{path} has no saved conversion for {cur}")
    acct = em["account"]
    slip_or_band = max(adverse / BPS, slip)
    return {
        "id": _string(raw.get("member_id"), path + ".member_id"),
        "provider": _string(raw.get("provider"), path + ".provider"),
        "key": tuple(em["key"][n].encode("utf-8") for n in _KEY_NAMES),
        "plain": plain,
        "capability": cap,
        "contract_size": contract_size,
        "lot": lot_native * contract_size,
        "min_base": pair_evidence.decimal_fraction(cmeta["min_qty"], "min_qty") * contract_size,
        "min_notional": pair_evidence.decimal_fraction(cmeta["min_notional"], "min_notional"),
        "price_currency": cmeta["price_currency"],
        "settlement_currency": cmeta["settlement_currency"],
        "price_rate": rates[cmeta["price_currency"]],
        "settle_rate": rates[cmeta["settlement_currency"]],
        "fee": fee,
        "slippage": slip,
        "rate": slip_or_band,
        "leverage": leverage,
        "imr": max(imr, 1 / leverage),
        "mmr": mmr,
        "equity": pair_evidence.decimal_fraction(acct["equity"], "equity"),
        "free": pair_evidence.decimal_fraction(acct["free_collateral"], "free_collateral"),
        "margin_used": pair_evidence.decimal_fraction(acct["margin_used"], "margin_used"),
        "booked": booked,
        "avg_px": avg,
        "asks": _levels(em["observation"]["asks"]),
        "bids": _levels(em["observation"]["bids"]),
    }


def _member_summary(m):
    return {
        "provider": m["provider"],
        "contract_size": _x(m["contract_size"]),
        "base_lot": _x(m["lot"]),
        "min_base_qty": _x(m["min_base"]),
        "min_notional": _x(m["min_notional"]),
        "price_currency": m["price_currency"],
        "price_rate_usd": _x(m["price_rate"]),
        "settlement_currency": m["settlement_currency"],
        "settlement_rate_usd": _x(m["settle_rate"]),
        "taker_fee": _x(m["fee"]),
        "slippage": _x(m["slippage"]),
        "adverse_rate": _x(m["rate"]),
        "leverage": _x(m["leverage"]),
        "initial_margin_rate": _x(m["imr"]),
        "maintenance_margin_rate": _x(m["mmr"]),
        "equity": _x(m["equity"]),
        "free_collateral": _x(m["free"]),
        "margin_used": _x(m["margin_used"]),
        "booked_base_qty": _x(m["booked"]),
        "booked_avg_px": _x(m["avg_px"]),
    }


def _context(request, env, params, decision):
    expect = pair_inputs.expectation(request)
    plain = {m["member_id"]: m for m in expect["members"]}
    lifecycle = _string(request.get("lifecycle"), "lifecycle")
    limits_raw = _object(request.get("limits"), "limits")
    limits = {}
    for k in LIMIT_FIELDS:
        limits[k] = _number(limits_raw.get(k), "limits." + k)
        if limits[k] <= 0:
            raise PolicyInvalid("request", f"limits.{k} must be above zero")
    raw_members = request.get("members")
    if not isinstance(raw_members, list) or len(raw_members) != 2:
        raise PolicyInvalid("request", "the pair request needs exactly two members")
    env_members = {m["key"]["member_id"]: m for m in env["members"]}
    members = []
    for i, raw in enumerate(raw_members):
        path = f"members[{i}]"
        mid = _string(_object(raw, path).get("member_id"), path + ".member_id")
        if mid not in env_members or mid not in plain:
            raise PolicyInvalid("evidence", f"member {mid} has no verified envelope record")
        members.append(_member(raw, plain[mid], env_members[mid], params["adverse_band_bps"], path))
    if sorted(m["provider"] for m in members) != sorted((PROVIDER_HYPERLIQUID, PROVIDER_OKX)):
        raise PolicyInvalid("request", "the pair needs one hyperliquid member and one okx member")
    decision["lifecycle"] = lifecycle
    decision["limits"] = {k: _x(v) for k, v in limits.items()}
    decision["members"] = {m["id"]: _member_summary(m) for m in members}
    return {"env": env, "params": params, "limits": limits, "members": members, "lifecycle": lifecycle}


def _lcm(a, b):
    return Fraction(math.lcm(a.numerator, b.numerator), math.gcd(a.denominator, b.denominator))


def _float_down(x):
    f = float(x)
    while math.isfinite(f) and f > 0 and (Fraction(f) > x or Fraction(pair_evidence.float_decimal(f)) > x):
        f = math.nextafter(f, 0.0)
    if not math.isfinite(f) or f <= 0:
        raise PairEvidenceError(f"bound {_x(x)} has no positive finite float at or below it")
    return f


def _float_up(x):
    f = float(x)
    while math.isfinite(f) and (Fraction(f) < x or Fraction(pair_evidence.float_decimal(f)) < x):
        f = math.nextafter(f, math.inf)
    if not math.isfinite(f) or f <= 0:
        raise PairEvidenceError(f"bound {_x(x)} has no positive finite float at or above it")
    return f


def _bound(m, side, proposed, cache):
    key = (m["id"], side, proposed)
    if key in cache:
        return cache[key]
    try:
        if proposed <= 0:
            raise PairEvidenceError(f"the proposed {side} bound {_x(proposed)} is not positive")
        emitted = _float_down(proposed) if side == "buy" else _float_up(proposed)
        rounded = Fraction(pair_evidence.float_decimal(pair_evidence.round_bound(m["plain"], m["capability"], side, emitted)))
        if (side == "buy" and rounded > proposed) or (side == "sell" and rounded < proposed):
            raise PairEvidenceError(f"the rounded {side} bound {_x(rounded)} widens the exact proposed bound {_x(proposed)}")
        out = (emitted, rounded, None)
    except PairEvidenceError as exc:
        out = (None, None, str(exc))
    cache[key] = out
    return out


def _reach(env, m, side, rounded):
    if not pair_inputs.bound_crosses(env, m["id"], side, float(rounded)):
        return Fraction(0)
    if side == "buy":
        return sum((qty for px, qty in m["asks"] if px <= rounded), Fraction(0))
    return sum((qty for px, qty in m["bids"] if px >= rounded), Fraction(0))


def _walk(levels, q, factor):
    remaining = q
    total = Fraction(0)
    worst = None
    for px, qty in levels:
        if remaining <= 0:
            break
        take = min(qty, remaining)
        total += take * px * factor
        remaining -= take
        worst = px
    if remaining > 0 or worst is None:
        return None
    return total, worst


def _cumulative(levels):
    out = []
    total = Fraction(0)
    for _, qty in levels:
        total += qty
        out.append(total)
    return out


class _Range:
    def __init__(self, lo, hi):
        self.lo = (lo, False)
        self.hi = (hi, True)
        self.cuts = []
        self.never = None

    def lower(self, value, inclusive, name):
        v, inc = self.lo
        if value > v or (value == v and inc and not inclusive):
            self.lo = (value, inclusive)
            self.cuts.append(name)

    def upper(self, value, inclusive, name):
        v, inc = self.hi
        if value < v or (value == v and inc and not inclusive):
            self.hi = (value, inclusive)
            self.cuts.append(name)

    def impossible(self, name):
        if self.never is None:
            self.never = name

    def positive(self, c0, c1, strict, name):
        if c1 > 0:
            self.lower(-c0 / c1, not strict, name)
        elif c1 < 0:
            self.upper(-c0 / c1, not strict, name)
        elif c0 < 0 or (strict and c0 == 0):
            self.impossible(name)

    def largest(self, grid):
        if self.never is not None:
            return None
        hv, hinc = self.hi
        k = math.floor(hv / grid)
        if not hinc and k * grid == hv:
            k -= 1
        if k < 1:
            return None
        q = k * grid
        lv, linc = self.lo
        if q < lv or (q == lv and not linc):
            return None
        return q


def _affine(levels, cum, hi, factor):
    k = next(i for i, c in enumerate(cum) if c >= hi)
    before = cum[k - 1] if k else Fraction(0)
    slope = levels[k][0] * factor
    base = sum((qty * px * factor for px, qty in levels[:k]), Fraction(0)) - before * slope
    return base, slope, levels[k][0]


def _entry_terms(ctx):
    return ctx["params"]["entry_threshold_bps"], ctx["params"]["funding_allowance_bps"] / BPS


def _solve_interval(ctx, buy, sell, lo, hi, grid, cache):
    env = ctx["env"]
    limits = ctx["limits"]
    entry, funding = _entry_terms(ctx)
    buy_factor = (1 + buy["rate"]) * buy["price_rate"]
    sell_factor = (1 - sell["rate"]) * sell["price_rate"]
    b0, b1, worst_ask = _affine(buy["asks"], _cumulative(buy["asks"]), hi, buy_factor)
    s0, s1, worst_bid = _affine(sell["bids"], _cumulative(sell["bids"]), hi, sell_factor)
    rng = _Range(lo, hi)
    keep = 1 - 2 * sell["fee"] - sell["rate"]
    give = 1 + 2 * buy["fee"] + buy["rate"] + funding
    k0 = s0 * keep - b0 * give
    k1 = s1 * keep - b1 * give
    rng.positive(k0, k1, True, "capture")
    rng.positive(BPS * k0 - entry * b0, BPS * k1 - entry * b1, True, "entry_threshold")
    proposed_buy = worst_ask * (1 + buy["rate"])
    proposed_sell = worst_bid * (1 - sell["rate"])
    eb, rb, why_b = _bound(buy, "buy", proposed_buy, cache)
    es, rs, why_s = _bound(sell, "sell", proposed_sell, cache)
    summary = {"lo": _x(lo), "hi": _x(hi), "worst_ask": _x(worst_ask), "worst_bid": _x(worst_bid),
               "proposed_buy_bound": _x(proposed_buy), "proposed_sell_bound": _x(proposed_sell)}
    if why_b or why_s:
        rng.impossible("rounding")
        summary["rounding_error"] = why_b or why_s
    else:
        summary["rounded_buy_bound"] = _x(rb)
        summary["rounded_sell_bound"] = _x(rs)
        for m in (buy, sell):
            rng.lower(m["min_base"], True, "min_qty:" + m["id"])
        notional_floor_buy = min(proposed_buy, rb)
        notional_floor_sell = min(proposed_sell, rs)
        if buy["min_notional"] > 0:
            rng.lower(buy["min_notional"] / notional_floor_buy, True, "min_notional:" + buy["id"])
        if sell["min_notional"] > 0:
            rng.lower(sell["min_notional"] / notional_floor_sell, True, "min_notional:" + sell["id"])
        rng.upper(_reach(env, buy, "buy", rb), True, "reach:" + buy["id"])
        rng.upper(_reach(env, sell, "sell", rs), True, "reach:" + sell["id"])
        unit_buy = max(proposed_buy, rb) * buy["price_rate"]
        unit_sell = max(proposed_sell, rs) * sell["price_rate"]
        rng.upper(limits["gross_budget_usd"] / (unit_buy + unit_sell), True, "gross_budget")
        venue = {}
        for m, unit in ((buy, unit_buy), (sell, unit_sell)):
            venue[m["provider"]] = venue.get(m["provider"], Fraction(0)) + unit
        for provider in sorted(venue):
            rng.upper(limits[VENUE_CAP_FIELDS[provider]] / venue[provider], True, "venue_cap:" + provider)
        for m, unit in ((buy, unit_buy), (sell, unit_sell)):
            rng.upper(m["free"] * m["settle_rate"] / (unit * (m["imr"] + m["fee"])), True, "collateral:" + m["id"])
            rng.upper(m["equity"] * m["settle_rate"] / (unit * (m["mmr"] + m["fee"])), True, "maintenance:" + m["id"])
    q = rng.largest(grid)
    summary["binding_constraints"] = sorted(set(rng.cuts))
    if rng.never is not None:
        summary["impossible"] = rng.never
    summary["selected_qty"] = _x(q) if q is not None else None
    return q, summary


def entry_candidate(ctx, buy, sell, q, cache=None):
    cache = {} if cache is None else cache
    env = ctx["env"]
    limits = ctx["limits"]
    entry, funding = _entry_terms(ctx)
    grid = _lcm(buy["lot"], sell["lot"])
    failed = []
    out = {"qty": _x(q), "grid": _x(grid)}
    walked_buy = _walk(buy["asks"], q, (1 + buy["rate"]) * buy["price_rate"])
    walked_sell = _walk(sell["bids"], q, (1 - sell["rate"]) * sell["price_rate"])
    if walked_buy is None or walked_sell is None:
        out.update({"passed": False, "failed": ["depth"]})
        return out
    b, worst_ask = walked_buy
    s, worst_bid = walked_sell
    entry_fees = buy["fee"] * b + sell["fee"] * s
    exit_fee_reserve = buy["fee"] * b + sell["fee"] * s
    exit_adverse_reserve = buy["rate"] * b + sell["rate"] * s
    funding_reserve = b * funding
    capture = s - b - entry_fees - exit_fee_reserve - exit_adverse_reserve - funding_reserve
    if not capture > 0:
        failed.append("capture")
    if not BPS * capture > entry * b:
        failed.append("entry_threshold")
    if (q / grid).denominator != 1 or (q / buy["lot"]).denominator != 1 or (q / sell["lot"]).denominator != 1 or q <= 0:
        failed.append("lot")
    for m in (buy, sell):
        if q < m["min_base"]:
            failed.append("min_qty:" + m["id"])
    proposed_buy = worst_ask * (1 + buy["rate"])
    proposed_sell = worst_bid * (1 - sell["rate"])
    eb, rb, why_b = _bound(buy, "buy", proposed_buy, cache)
    es, rs, why_s = _bound(sell, "sell", proposed_sell, cache)
    out.update({
        "buy_member": buy["id"], "sell_member": sell["id"],
        "buy_native_qty": _x(q / buy["contract_size"]), "sell_native_qty": _x(q / sell["contract_size"]),
        "buy_cost_usd": _x(b), "sell_proceeds_usd": _x(s), "worst_ask": _x(worst_ask), "worst_bid": _x(worst_bid),
        "entry_fees_usd": _x(entry_fees), "exit_fee_reserve_usd": _x(exit_fee_reserve),
        "exit_adverse_reserve_usd": _x(exit_adverse_reserve), "funding_reserve_usd": _x(funding_reserve),
        "capture_usd": _x(capture), "capture_bps": _x(BPS * capture / b),
        "proposed_buy_bound": _x(proposed_buy), "proposed_sell_bound": _x(proposed_sell),
    })
    if why_b or why_s:
        failed.append("rounding")
        out["rounding_error"] = why_b or why_s
    else:
        out.update({"rounded_buy_bound": _x(rb), "rounded_sell_bound": _x(rs), "buy_price_bound": eb, "sell_price_bound": es})
        for m, side, proposed, rounded in ((buy, "buy", proposed_buy, rb), (sell, "sell", proposed_sell, rs)):
            if m["min_notional"] > 0 and q * min(proposed, rounded) < m["min_notional"]:
                failed.append("min_notional:" + m["id"])
            if _reach(env, m, side, rounded) < q:
                failed.append("reach:" + m["id"])
        notional_buy = q * max(proposed_buy, rb) * buy["price_rate"]
        notional_sell = q * max(proposed_sell, rs) * sell["price_rate"]
        gross = notional_buy + notional_sell
        if not gross <= limits["gross_budget_usd"]:
            failed.append("gross_budget")
        venue = {}
        for m, n in ((buy, notional_buy), (sell, notional_sell)):
            venue[m["provider"]] = venue.get(m["provider"], Fraction(0)) + n
        for provider in sorted(venue):
            if not venue[provider] <= limits[VENUE_CAP_FIELDS[provider]]:
                failed.append("venue_cap:" + provider)
        margin = {}
        for m, n in ((buy, notional_buy), (sell, notional_sell)):
            fee_reserve = m["fee"] * n
            required = n * m["imr"] + fee_reserve
            available = m["free"] * m["settle_rate"]
            maintenance = n * m["mmr"]
            equity_after = m["equity"] * m["settle_rate"] - fee_reserve
            if available < required:
                failed.append("collateral:" + m["id"])
            if equity_after < maintenance:
                failed.append("maintenance:" + m["id"])
            margin[m["id"]] = {"notional_usd": _x(n), "required_usd": _x(required), "available_usd": _x(available),
                               "maintenance_usd": _x(maintenance), "equity_after_fees_usd": _x(equity_after)}
        out.update({"gross_usd": _x(gross), "venue_usd": {k: _x(v) for k, v in venue.items()}, "unmatched_usd": "0", "margin": margin})
    out["passed"] = not failed
    out["failed"] = failed
    return out


def _search(ctx, buy, sell, cache):
    grid = _lcm(buy["lot"], sell["lot"])
    cum_buy = _cumulative(buy["asks"])
    cum_sell = _cumulative(sell["bids"])
    depth = min(cum_buy[-1], cum_sell[-1]) if cum_buy and cum_sell else Fraction(0)
    points = sorted({c for c in cum_buy + cum_sell if c <= depth})
    intervals = []
    prev = Fraction(0)
    for p in points:
        intervals.append((prev, p))
        prev = p
    summaries = []
    selected = None
    checked = 0
    for lo, hi in reversed(intervals):
        q, summary = _solve_interval(ctx, buy, sell, lo, hi, grid, cache)
        checked += 1
        if len(summaries) < MAX_SUMMARIES:
            summaries.append(summary)
        if q is not None:
            candidate = entry_candidate(ctx, buy, sell, q, cache)
            if not candidate["passed"]:
                raise RuntimeError(f"the interval solution {q} failed its direct recomputation: {candidate['failed']}")
            selected = (q, candidate)
            break
    orientation = ORIENTATIONS[(buy["provider"], sell["provider"])]
    report = {"orientation": orientation, "buy_member": buy["id"], "sell_member": sell["id"], "grid": _x(grid),
              "saved_depth": _x(depth), "intervals": len(intervals), "intervals_checked": checked, "interval_summaries": summaries}
    deepest_k = math.floor(depth / grid) if grid > 0 else 0
    if deepest_k >= 1:
        deep = entry_candidate(ctx, buy, sell, deepest_k * grid, cache)
        report["deepest"] = {"qty": deep["qty"], "passed": deep["passed"], "failed": deep["failed"]}
    if selected is not None:
        report["selected_qty"] = _x(selected[0])
    return orientation, selected, report


def _entry(ctx, decision):
    members = ctx["members"]
    booked = [m for m in members if m["booked"] != 0]
    if booked:
        raise PolicyHold("entry", "the pair lifecycle is flat but " + ", ".join(f"{m['id']} books {_x(m['booked'])}" for m in booked))
    cache = {}
    results = []
    reports = []
    for buy, sell in ((members[0], members[1]), (members[1], members[0])):
        orientation, selected, report = _search(ctx, buy, sell, cache)
        reports.append(report)
        if selected is not None:
            results.append((selected[0], buy, sell, orientation, selected[1]))
    decision["orientations"] = reports
    if not results:
        detail = "; ".join(f"{r['orientation']}: deepest {r.get('deepest', {}).get('qty', 'none')} fails {','.join(r.get('deepest', {}).get('failed', [])) or 'nothing'}" for r in reports)
        raise PolicyHold("entry", f"no quantity passes every entry condition in either orientation ({detail})")
    results.sort(key=lambda r: (-r[0], r[1]["key"], r[2]["key"]))
    q, buy, sell, orientation, candidate = results[0]
    decision["stage"] = "entry"
    decision["selected"] = candidate
    decision["selected_orientation"] = orientation
    decision["reason"] = f"{orientation} {candidate['qty']} base passes every entry condition at {candidate['capture_bps']} bps capture"
    return {
        "status": "enter",
        "orientation": orientation,
        "targets": {
            buy["id"]: {"target_base_qty": float(q), "price_bound": candidate["buy_price_bound"]},
            sell["id"]: {"target_base_qty": -float(q), "price_bound": candidate["sell_price_bound"]},
        },
    }


def _exit(ctx, decision):
    members = ctx["members"]
    params = ctx["params"]
    env = ctx["env"]
    longs = [m for m in members if m["booked"] > 0]
    shorts = [m for m in members if m["booked"] < 0]
    if len(longs) != 1 or len(shorts) != 1:
        raise PolicyHold("exit", "a matched pair needs one long and one short booked member, got " +
                         ", ".join(f"{m['id']} {_x(m['booked'])}" for m in members))
    long_m, short_m = longs[0], shorts[0]
    q_long = long_m["booked"]
    q_short = -short_m["booked"]
    info = {"long_member": long_m["id"], "short_member": short_m["id"], "long_qty": _x(q_long), "short_qty": _x(q_short),
            "qty_difference": _x(q_long - q_short),
            "booked_orientation": ORIENTATIONS[(long_m["provider"], short_m["provider"])]}
    decision["exit"] = info
    sale = _walk(long_m["bids"], q_long, (1 - long_m["rate"]) * long_m["price_rate"])
    cover = _walk(short_m["asks"], q_short, (1 + short_m["rate"]) * short_m["price_rate"])
    if sale is None or cover is None:
        raise PolicyHold("exit", "the saved closing depth cannot cover the booked quantities; coordinator protection and recovery continue")
    proceeds, worst_bid = sale
    cost, worst_ask = cover
    unit_sale = proceeds / q_long
    unit_cover = cost / q_short
    spread = BPS * (unit_cover - unit_sale) / unit_sale
    close_fees = long_m["fee"] * proceeds + short_m["fee"] * cost
    net = proceeds - cost - close_fees
    pnl = (proceeds - q_long * long_m["avg_px"] * long_m["price_rate"]) + (q_short * short_m["avg_px"] * short_m["price_rate"] - cost) - close_fees
    proposed_sell = worst_bid * (1 - long_m["rate"])
    proposed_buy = worst_ask * (1 + short_m["rate"])
    cache = {}
    es, rs, why_s = _bound(long_m, "sell", proposed_sell, cache)
    eb, rb, why_b = _bound(short_m, "buy", proposed_buy, cache)
    info.update({"long_sale_proceeds_usd": _x(proceeds), "short_cover_cost_usd": _x(cost), "remaining_spread_bps": _x(spread),
                 "exit_threshold_bps": _x(params["exit_threshold_bps"]), "close_fees_usd": _x(close_fees),
                 "net_liquidation_usd": _x(net), "booked_pnl_estimate_usd": _x(pnl),
                 "proposed_long_sell_bound": _x(proposed_sell), "proposed_short_buy_bound": _x(proposed_buy)})
    if why_s or why_b:
        raise PolicyHold("exit", f"the closing bounds cannot be rounded: {why_s or why_b}")
    info.update({"rounded_long_sell_bound": _x(rs), "rounded_short_buy_bound": _x(rb)})
    if _reach(env, long_m, "sell", rs) < q_long or _reach(env, short_m, "buy", rb) < q_short:
        raise PolicyHold("exit", "the rounded closing bounds cannot reach the saved depth for the booked quantities; coordinator protection and recovery continue")
    if spread > params["exit_threshold_bps"]:
        raise PolicyHold("exit", f"the remaining spread {_x(spread)} bps is above the exit threshold {_x(params['exit_threshold_bps'])} bps")
    decision["stage"] = "exit"
    decision["reason"] = f"the remaining spread {_x(spread)} bps is at or below the exit threshold {_x(params['exit_threshold_bps'])} bps"
    return {
        "status": "exit",
        "targets": {
            long_m["id"]: {"target_base_qty": 0.0, "price_bound": es},
            short_m["id"]: {"target_base_qty": 0.0, "price_bound": eb},
        },
    }


def evaluate_pair(request: dict) -> dict:
    result = _identity(request)
    decision = {"policy": POLICY}
    try:
        params = _admit(request, decision)
        env = _verify(request, decision)
        ctx = _context(request, env, params, decision)
        if ctx["lifecycle"] == LIFECYCLE_FLAT:
            result.update(_entry(ctx, decision))
        elif ctx["lifecycle"] == LIFECYCLE_MATCHED:
            result.update(_exit(ctx, decision))
        else:
            raise PolicyHold("lifecycle", f"lifecycle {ctx['lifecycle']!r} has no policy decision; the coordinator keeps its authority")
    except PolicyInvalid as exc:
        decision["stage"] = exc.stage
        decision["reason"] = _clip(exc.reason)
        result.update({"status": "invalid", "error": _clip(exc.reason)})
    except PolicyHold as exc:
        decision["stage"] = exc.stage
        decision["reason"] = _clip(exc.reason)
        result.update({"status": "hold", "hold_reason": _clip(exc.reason)})
    result["decision"] = decision
    return result
