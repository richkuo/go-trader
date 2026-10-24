import importlib.util
import os
import sys

PAIR_CONTRACT = {"input": "pair_policy_request_v2", "output": "pair_policy_result_v2"}

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.dirname(os.path.dirname(_OPEN_DIR))

_PAIR_DESCRIPTORS = {
    "cross_exchange_perp_arbitrage": {
        "module": "go_trader_pair_policy_cross_exchange_perp_arbitrage",
        "path": os.path.join("shared_strategies", "open", "cross_exchange_perp_arbitrage.py"),
        "param_keys": ("adverse_band_bps", "entry_threshold_bps", "exit_threshold_bps", "funding_allowance_bps"),
        "description": (
            "RESEARCH, production-refused pair policy (#1875, #1878, parent #1655): one cross-exchange perpetual spread "
            "between a Hyperliquid perps member and an OKX swap member for one underlying, decided once per cadence slot "
            "from the frozen version-2 pair request (saved two-venue order books, accounts, conversions, member costs and "
            "limits). It sizes the largest common-lot quantity whose walked spread capture after entry fees, the exit-fee "
            "reserve, the exit adverse reserve and the funding allowance clears entry_threshold_bps, and exits a matched "
            "pair when the remaining spread is at or below exit_threshold_bps. It has no single-symbol signal: scalar "
            "checks, candle backtests, the optimizer and parity runs refuse it, and only the internally admitted research "
            "pair coordinator runs it through shared_scripts/run_pair_policy.py; every production config, mode and "
            "allow_no_edge acknowledgement is refused until a separate reviewed promotion"
        ),
        "platforms": ("futures",),
        "short_entries": False,
        "edge_status": "no_edge",
        "edge_source": "unvalidated",
        "edge_ref": "backtest/candidates/cross_exchange_perp_arbitrage_1655/REPORT.md",
        "production_refused": True,
    },
}


class UnknownPairPolicy(LookupError):
    pass


def pair_policies():
    return tuple(sorted(_PAIR_DESCRIPTORS))


def pair_descriptor(name):
    descriptor = _PAIR_DESCRIPTORS.get(name) if isinstance(name, str) else None
    if descriptor is None:
        raise UnknownPairPolicy(f"{name!r} is not a registered pair policy; registered: {list(pair_policies())}")
    return dict(descriptor, contract=dict(PAIR_CONTRACT))


def _load(module_name, rel):
    path = os.path.join(_ROOT, rel)
    loaded = sys.modules.get(module_name)
    if loaded is not None:
        origin = getattr(loaded, "__file__", None) or ""
        if os.path.realpath(origin) != os.path.realpath(path):
            raise ImportError(f"module {module_name} is loaded from {origin!r}, not {path!r}")
        return loaded
    spec = importlib.util.spec_from_file_location(module_name, path)
    if spec is None or spec.loader is None:
        raise ImportError(f"pair policy module {module_name} has no loader at {path!r}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    previous = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(module_name, None)
        raise
    finally:
        sys.dont_write_bytecode = previous
    return module


def get_pair_evaluator(name):
    descriptor = pair_descriptor(name)
    module = _load(descriptor["module"], descriptor["path"])
    evaluate = getattr(module, "evaluate_pair", None)
    if not callable(evaluate):
        raise ImportError(f"pair policy module {descriptor['module']} defines no callable evaluate_pair")
    return evaluate
