import copy
import importlib.util
import json
import os
import re
import sys

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SCHEDULER = os.path.join(REPO, "scheduler")


def _load(module_name, *parts):
    spec = importlib.util.spec_from_file_location(module_name, os.path.join(REPO, *parts))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


OPEN_REGISTRY = _load("_parity_open_registry", "shared_strategies", "open", "registry.py")
CLOSE_REGISTRY = _load("_parity_close_registry", "shared_strategies", "close", "registry.py")


def _go_source(name):
    with open(os.path.join(SCHEDULER, name)) as fh:
        return fh.read()


def _go_block(source, header):
    start = source.index(header) + len(header)
    end = source.index("\n}\n", start)
    return source[start:end]


def _go_string_consts(source):
    return dict(re.findall(r'^\s*(\w+)\s*=\s*"([^"]*)"\s*$', source, re.M))


def go_no_edge_metadata():
    source = _go_source("edge_status.go")
    consts = _go_string_consts(source)
    block = _go_block(source, "var noEdgeStrategies = map[string]noEdgeEvidence{")
    out = {}
    for name, src, ref in re.findall(r'^\s*"(\w+)":\s*\{"(\w+)",\s*("[^"]*"|\w+)\},\s*$', block, re.M):
        out[name] = (src, ref.strip('"') if ref.startswith('"') else consts[ref])
    return out


def go_registered_platforms():
    block = _go_block(_go_source("init.go"), "var registeredOpenStrategyPlatforms = map[string][]string{")
    return {name: tuple(re.findall(r'"(\w+)"', plats))
            for name, plats in re.findall(r'^\s*"(\w+)":\s*\{([^}]*)\},\s*$', block, re.M)}


def go_short_names():
    block = _go_block(_go_source("init.go"), "var knownShortNames = map[string]string{")
    return dict(re.findall(r'^\s*"(\w+)":\s*"(\w+)",\s*$', block, re.M))


def go_bidirectional_perps():
    block = _go_block(_go_source("init.go"), "var bidirectionalPerpsStrategies = map[string]bool{")
    entries = re.findall(r'^\s*"(\w+)":\s*(true|false),\s*$', block, re.M)
    assert len(entries) == len([line for line in block.splitlines() if line.strip()]), (
        "unparsed line in bidirectionalPerpsStrategies")
    return {name: True for name, value in entries if value == "true"}


def go_v20_former_research_strategies():
    block = _go_block(_go_source("config_migration_v20.go"), "var v20FormerResearchStrategies = map[string]struct{}{")
    entries = re.findall(r'^\s*"(\w+)":\s*\{\},\s*$', block, re.M)
    assert len(entries) == len([line for line in block.splitlines() if line.strip()]), (
        "unparsed line in v20FormerResearchStrategies")
    return set(entries)


def go_fixed_interval_defaults():
    block = _go_block(_go_source("fixed_interval_strategies.go"),
                      "var fixedIntervalStrategyDefaults = map[string]int64{")
    entries = re.findall(r'^\s*"(\w+)":\s*([0-9_]+),\s*$', block, re.M)
    assert len(entries) == len([line for line in block.splitlines() if line.strip()]), (
        "unparsed line in fixedIntervalStrategyDefaults")
    return {name: int(value.replace("_", "")) for name, value in entries}


def python_fixed_interval_defaults():
    return {
        name: entry["default_params"]["expected_interval_ms"]
        for name, entry in OPEN_REGISTRY.STRATEGIES.items()
        if "expected_interval_ms" in entry["default_params"]
    }


def python_short_entries():
    return {name: True for name in OPEN_REGISTRY.short_entry_strategies()}


def go_close_owned_keys():
    block = _go_block(_go_source("config_migration.go"),
                      "var closeStrategyOwnedKeys = map[string]map[string]struct{}{")
    return {name: set(re.findall(r'"(\w+)":\s*\{\}', keys))
            for name, keys in re.findall(r'^\s*"(\w+)":\s*\{(.*)\},\s*$', block, re.M)}


def python_no_edge_metadata():
    return {
        name: (entry["edge_source"], entry["edge_ref"])
        for name, entry in OPEN_REGISTRY.STRATEGIES.items()
        if entry.get("edge_status") == OPEN_REGISTRY.EDGE_STATUS_NO_EDGE
    }


def python_registered_platforms():
    return {name: tuple(entry["platforms"]) for name, entry in OPEN_REGISTRY.STRATEGIES.items()}


def mapping_mismatches(go, py):
    problems = []
    for name in sorted(set(go) | set(py)):
        if name not in go:
            problems.append(f"{name}: in Python, missing from Go")
        elif name not in py:
            problems.append(f"{name}: in Go, missing from Python")
        elif go[name] != py[name]:
            problems.append(f"{name}: Go {go[name]!r} != Python {py[name]!r}")
    return problems


def test_go_no_edge_metadata_equals_python_registry():
    go, py = go_no_edge_metadata(), python_no_edge_metadata()
    assert len(py) == 74
    assert mapping_mismatches(go, py) == []
    assert OPEN_REGISTRY.DISCOVERY_HIDDEN_STRATEGIES == frozenset(py)
    for source, ref in py.values():
        assert source in OPEN_REGISTRY.VALID_EDGE_SOURCES
        assert os.path.isfile(os.path.join(REPO, ref)), ref


@pytest.mark.parametrize("side", ["go", "python"])
def test_metadata_parity_detects_a_removed_name_or_changed_field(side):
    go, py = go_no_edge_metadata(), python_no_edge_metadata()
    target = go if side == "go" else py
    removed = copy.deepcopy(target)
    removed.pop("awesome_oscillator")
    changed = copy.deepcopy(target)
    changed["rsi"] = ("unvalidated", changed["rsi"][1])
    moved = copy.deepcopy(target)
    moved["vortex_trend"] = (moved["vortex_trend"][0], "docs/research/fee-audit-m5.md")
    for mutated in (removed, changed, moved):
        pair = (mutated, py) if side == "go" else (go, mutated)
        assert len(mapping_mismatches(*pair)) == 1


def go_production_refused():
    source = _go_source("production_refusal.go")
    block = _go_block(source, "var productionRefusedStrategies = map[string]string{")
    consts = {
        "cumulativeVolumeDeltaStrategyName": _go_string_consts(_go_source("market_feed_trades.go"))[
            "cumulativeVolumeDeltaStrategyName"],
        "bitcoinLeadLagStrategyName": _go_string_consts(_go_source("bitcoin_lead_lag_feed.go"))[
            "bitcoinLeadLagStrategyName"],
        "volumeProfileReclaimStrategyName": _go_string_consts(_go_source("market_feed_profile.go"))[
            "volumeProfileReclaimStrategyName"],
    }
    out = {}
    for key, ref in re.findall(r'^\s*(\w+|"\w+"):\s*"([^"]*)",\s*$', block, re.M):
        out[key.strip('"') if key.startswith('"') else consts[key]] = ref
    assert len(out) == len([line for line in block.splitlines() if line.strip()]), "unparsed productionRefusedStrategies line"
    return out


def test_go_production_refusal_equals_python_registry():
    go = go_production_refused()
    py = {name: OPEN_REGISTRY.STRATEGIES[name]["edge_ref"] for name in OPEN_REGISTRY.production_refused_strategies()}
    assert py == {
        "cumulative_volume_delta_divergence": "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md",
        "bitcoin_lead_lag": "backtest/candidates/bitcoin_lead_lag_1640/REPORT.md",
        "macro_event_breakout": "backtest/candidates/macro_event_breakout_1654/REPORT.md",
        "volume_profile_reclaim": "backtest/candidates/volume_profile_reclaim_1665/REPORT.md",
        "perpetual_premium_reversion": "backtest/candidates/perpetual_premium_reversion_1639/REPORT.md",
        "order_book_imbalance": "backtest/candidates/order_book_imbalance_1635/REPORT.md",
        "inventory_market_making": "backtest/candidates/inventory_market_making_1636/REPORT.md",
        "cross_exchange_perp_arbitrage": "backtest/candidates/cross_exchange_perp_arbitrage_1655/REPORT.md",
    }
    assert go == py
    for name in py:
        assert OPEN_REGISTRY.STRATEGIES[name]["edge_status"] == OPEN_REGISTRY.EDGE_STATUS_NO_EDGE


def test_v20_auto_acknowledgement_exclusions_are_registered_no_edge_names():
    excluded = go_v20_former_research_strategies()
    assert "chande_momentum" in excluded
    assert excluded <= set(python_no_edge_metadata())


def test_go_fixed_interval_defaults_equal_python_registry():
    go, py = go_fixed_interval_defaults(), python_fixed_interval_defaults()
    assert py.get("chande_momentum") == 3_600_000
    assert go == py


def test_go_calendar_training_default_equals_python_registry():
    source = _go_source("calendar_conditioned_entry_config.go")
    match = re.search(r"^\s*calendarConditionedEntryDefaultTrainingBars\s*=\s*([0-9_]+)\s*$", source, re.M)
    assert match, "calendarConditionedEntryDefaultTrainingBars not found"
    py = OPEN_REGISTRY.STRATEGIES["calendar_conditioned_entry"]["default_params"]["training_bars"]
    assert int(match.group(1).replace("_", "")) == py


def go_premium_lookback_bounds():
    source = _go_source("market_feed_premium.go")
    out = {}
    for key, name in (("default", "perpetualPremiumDefaultLookback"), ("max", "perpetualPremiumMaxLookback")):
        match = re.search(rf"^\s*{name}\s*=\s*([0-9_]+)\s*$", source, re.M)
        assert match, f"{name} not found"
        out[key] = int(match.group(1).replace("_", ""))
    return out


def python_premium_lookback_bounds():
    entry = OPEN_REGISTRY.STRATEGIES["perpetual_premium_reversion"]
    maxima = [int(m.group(1)) for c in entry["constraints"]
              for m in [re.fullmatch(r"\s*lookback_samples\s*<=\s*([0-9]+)\s*", c)] if m]
    assert len(maxima) == 1, f"expected one lookback_samples upper bound in constraints, got {maxima}"
    return {"default": entry["default_params"]["lookback_samples"], "max": maxima[0]}


def test_go_premium_lookback_default_and_maximum_equal_python_registry():
    go, py = go_premium_lookback_bounds(), python_premium_lookback_bounds()
    assert py == {"default": 30, "max": 720}
    assert go == py


def refused_in_both_languages():
    return set(go_production_refused()) & set(OPEN_REGISTRY.production_refused_strategies())


def unpromoted_absence(problems, names):
    return [p for p in problems if not any(p == f"{n}: in Python, missing from Go" for n in names)]


def test_go_bidirectional_perps_equal_python_short_entries():
    go, py = go_bidirectional_perps(), python_short_entries()
    assert py
    assert unpromoted_absence(mapping_mismatches(go, py), refused_in_both_languages()) == []
    assert "bitcoin_lead_lag" in py and "bitcoin_lead_lag" not in go
    assert "volume_profile_reclaim" in py and "volume_profile_reclaim" not in go
    assert "order_book_imbalance" in py and "order_book_imbalance" not in go


@pytest.mark.parametrize("side", ["go", "python"])
def test_short_entry_parity_detects_a_removed_name(side):
    go, py = go_bidirectional_perps(), python_short_entries()
    target = go if side == "go" else py
    removed = copy.deepcopy(target)
    removed.pop("awesome_oscillator")
    pair = (removed, py) if side == "go" else (go, removed)
    assert unpromoted_absence(mapping_mismatches(*pair), refused_in_both_languages()) == [
        "awesome_oscillator: in Python, missing from Go" if side == "go"
        else "awesome_oscillator: in Go, missing from Python"
    ]


@pytest.fixture
def scratch_registry():
    return _load("_parity_scratch_open_registry", "shared_strategies", "open", "registry.py")


def _noop(df):
    return df


@pytest.mark.parametrize("value", ["missing", 1, 0, None, "true"])
def test_register_requires_bool_short_entries(scratch_registry, value):
    kwargs = {} if value == "missing" else {"short_entries": value}
    before = set(scratch_registry.STRATEGIES)
    with pytest.raises((TypeError, ValueError), match="short_entries"):
        scratch_registry.register("_scratch", "scratch", {}, **kwargs)(_noop)
    assert set(scratch_registry.STRATEGIES) == before


@pytest.mark.parametrize("kwargs", [
    {"platforms": ("spot",), "short_entries": True},
    {"default_params": {"allow_short": True}, "short_entries": False},
    {"variants": {"futures": {"default_params": {"allow_short": True}}}, "short_entries": False},
])
def test_register_rejects_inconsistent_short_entries(scratch_registry, kwargs):
    kwargs = {"default_params": {"allow_short": False}, **kwargs}
    before = set(scratch_registry.STRATEGIES)
    with pytest.raises(ValueError, match="short_entries"):
        scratch_registry.register("_scratch", "scratch", **kwargs)(_noop)
    assert set(scratch_registry.STRATEGIES) == before


@pytest.mark.parametrize("short_entries", [True, False])
def test_register_stores_declared_short_entries(scratch_registry, short_entries):
    scratch_registry.register("_scratch", "scratch", {"allow_short": short_entries},
                              short_entries=short_entries)(_noop)
    assert scratch_registry.STRATEGIES["_scratch"]["short_entries"] is short_entries
    assert ("_scratch" in scratch_registry.short_entry_strategies()) is short_entries


def test_go_registered_platforms_equal_python_registry():
    go, py = go_registered_platforms(), python_registered_platforms()
    assert len(py) == len(OPEN_REGISTRY.STRATEGIES)
    assert mapping_mismatches(go, py) == []


def test_every_registered_open_strategy_has_a_unique_short_name():
    short = go_short_names()
    assert set(OPEN_REGISTRY.STRATEGIES) - set(short) <= refused_in_both_languages()
    assert "bitcoin_lead_lag" not in short and "cumulative_volume_delta_divergence" in short
    assert "volume_profile_reclaim" not in short
    assert "order_book_imbalance" not in short
    values = list(short.values())
    assert len(values) == len(set(values))


def test_go_native_close_names_mirror_python_close_registry():
    owned = go_close_owned_keys()
    assert set(owned) == set(CLOSE_REGISTRY.STRATEGIES) | {"tp_at_pct"}
    for name, entry in CLOSE_REGISTRY.STRATEGIES.items():
        missing = set(entry["default_params"]) - owned[name]
        assert missing == set(), (
            f"close strategy {name!r} has default_params {sorted(missing)} missing from "
            "closeStrategyOwnedKeys; legacy migrations would route those params to the open ref")
    assert set(owned) & set(OPEN_REGISTRY.STRATEGIES) == set()


def go_research_only_closes():
    block = _go_block(_go_source("quote_config.go"), "var researchOnlyCloseStrategies = map[string]bool{")
    names = set()
    for line in block.splitlines():
        line = line.strip()
        if not line:
            continue
        m = re.match(r"^(\w+|\"\w+\"):\s*true,$", line)
        assert m, f"unparsed line in researchOnlyCloseStrategies: {line!r}"
        names.add(m.group(1).strip('"'))
    consts = _go_string_consts(_go_source("quote_config.go"))
    return {consts.get(n, n) for n in names}


def test_go_research_only_closes_equal_python_close_registry():
    go = go_research_only_closes()
    py = CLOSE_REGISTRY.research_only_close_names()
    assert go == py == {"inventory_quote_exit"}
    for name in py:
        for platform in CLOSE_REGISTRY.VALID_PLATFORMS:
            assert name not in CLOSE_REGISTRY.build_close_registry(platform)
        with pytest.raises(ValueError, match="research-only"):
            CLOSE_REGISTRY.evaluate(name, {}, {}, {})


def go_quote_capabilities():
    block = _go_block(_go_source("quote_config.go"), "var quoteCapabilityStrategies = map[string]quoteCapabilitySpec{")
    out = {}
    for line in block.splitlines():
        line = line.strip()
        if not line:
            continue
        m = re.match(r'^"(\w+)":\s*\{Version:\s*(\w+),\s*Validate:\s*(\w+)\},$', line)
        assert m, f"unparsed line in quoteCapabilityStrategies: {line!r}"
        out[m.group(1)] = (m.group(2), m.group(3))
    return out


def go_quote_policy_spec():
    source = _go_source("quote_policy_spec.go")
    blocks = re.findall(r"^const quotePolicySpecJSON = `([^`]*)`$", source, re.M | re.S)
    assert len(blocks) == 1, "quotePolicySpecJSON must be declared exactly once"
    return json.loads(blocks[0])


def test_go_quote_capabilities_equal_python_registry():
    go = go_quote_capabilities()
    py = OPEN_REGISTRY.quote_capabilities()
    assert set(go) == set(py) == {"inventory_market_making"}
    assert go["inventory_market_making"] == ("quotePolicySpecVersion", "validateInventoryMarketMakingParams")
    version = re.search(r"^\s*quotePolicySpecVersion\s*=\s*(\d+)\s*$", _go_source("quote_policy_spec.go"), re.M)
    spec = go_quote_policy_spec()
    for name, entry in py.items():
        assert entry["spec"] == spec
        assert entry["spec_version"] == spec["version"] == int(version.group(1))
        assert entry["quote"] == OPEN_REGISTRY.QUOTE_CONTRACT == spec["capability"]
        assert entry["platforms"] == ["futures"] and entry["production_refused"] is True
        assert entry["default_params"] == {f["name"]: f["default"] for f in spec["params"]}
        assert name in OPEN_REGISTRY.build_registry("futures", include_hidden=True)
        assert name not in OPEN_REGISTRY.build_registry("futures")
        assert name not in OPEN_REGISTRY.build_registry("spot", include_hidden=True)
        assert name not in OPEN_REGISTRY.short_entry_strategies() and name not in OPEN_REGISTRY.basket_rankers()


def test_quote_capabilities_refuse_every_scalar_path():
    loader = _load("_parity_quote_registry_loader", "backtest", "registry_loader.py")
    shim = loader.load_registry("futures")
    if os.path.join(REPO, "shared_tools") not in sys.path:
        sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    composition = importlib.import_module("strategy_composition")
    for name in sorted(OPEN_REGISTRY.quote_capabilities()):
        with pytest.raises(ValueError, match="quote capability"):
            OPEN_REGISTRY.STRATEGIES[name]["fn"](None)
        with pytest.raises(ValueError, match="quote capability"):
            loader.refuse_quote_capability(shim, name)
        with pytest.raises(ValueError, match="quote capability"):
            loader.refuse_basket_evaluator(shim, name)
        entry = dict(OPEN_REGISTRY.STRATEGIES[name], production_refused=False)
        with pytest.raises(ValueError, match="quote capability"):
            composition.admit_strategy_references([composition.StrategyReference("open", name)],
                                                  composition.GateMode(composition.GATE_MODE_PAPER, ""),
                                                  composition.Acknowledgement(None, None), lambda _n: entry)


@pytest.mark.parametrize("kwargs, match", [
    ({"quote": {"input": "deadline_book_evidence"}}, "quote must be exactly"),
    ({"quote_spec": {}}, "quote_spec applies only"),
    ({"platforms": ("spot", "futures")}, "platforms"),
    ({"production_refused": False, "edge_status": None, "edge_source": None, "edge_ref": None}, "production_refused"),
    ({"param_validator": None}, "param_validator"),
])
def test_register_rejects_malformed_quote_metadata(scratch_registry, kwargs, match):
    base = {"default_params": {}, "short_entries": False, "platforms": ("futures",), "edge_status": "no_edge",
            "edge_source": "unvalidated", "edge_ref": "x.md", "production_refused": True,
            "param_validator": lambda p: None, "quote": dict(OPEN_REGISTRY.QUOTE_CONTRACT),
            "quote_spec": {"strategy": "_scratch_quote"}}
    if "quote_spec" in kwargs and kwargs["quote_spec"] == {}:
        base.pop("quote")
    kwargs = {**base, **kwargs}
    before = set(scratch_registry.STRATEGIES)
    with pytest.raises(ValueError, match=match):
        scratch_registry.register("_scratch_quote", "scratch", **kwargs)(_noop)
    assert set(scratch_registry.STRATEGIES) == before


def go_basket_rankers():
    block = _go_block(_go_source("basket_config.go"), "var basketRankerStrategies = map[string]bool{")
    entries = re.findall(r'^\s*"(\w+)":\s*(true|false),\s*$', block, re.M)
    assert len(entries) == len([line for line in block.splitlines() if line.strip()]), (
        "unparsed line in basketRankerStrategies")
    return {name: True for name, value in entries if value == "true"}


def go_basket_ranker_param_keys():
    block = _go_block(_go_source("basket_config.go"), "var basketRankerParamKeys = map[string][]string{")
    return {name: set(re.findall(r'"(\w+)"', keys))
            for name, keys in re.findall(r'^\s*"(\w+)":\s*\{([^}]*)\},\s*$', block, re.M)}


def python_basket_rankers():
    return {name: True for name in OPEN_REGISTRY.basket_rankers()}


def test_go_basket_rankers_equal_python_registry():
    go, py = go_basket_rankers(), python_basket_rankers()
    assert py == {"basket_scripted_fixture": True, "cross_sectional_momentum": True, "cross_sectional_reversal": True}
    assert mapping_mismatches(go, py) == []
    for name in py:
        entry = OPEN_REGISTRY.STRATEGIES[name]
        assert entry["basket"] == OPEN_REGISTRY.BASKET_CONTRACT
        assert entry["platforms"] == ("futures",)
        assert entry["edge_status"] == OPEN_REGISTRY.EDGE_STATUS_NO_EDGE


def test_go_basket_ranker_param_keys_equal_python_defaults():
    go = go_basket_ranker_param_keys()
    py = {name: set(OPEN_REGISTRY.STRATEGIES[name]["default_params"]) for name in OPEN_REGISTRY.basket_rankers()}
    assert go == py


def test_basket_rankers_stay_out_of_every_single_symbol_registry():
    for platform in OPEN_REGISTRY.VALID_PLATFORMS:
        for hidden in (True, False):
            assert set(OPEN_REGISTRY.build_registry(platform, include_hidden=hidden)).isdisjoint(OPEN_REGISTRY.basket_rankers())
    loader = _load("_parity_registry_loader", "backtest", "registry_loader.py")
    shim = loader.load_registry("futures")
    for name in sorted(OPEN_REGISTRY.basket_rankers()):
        entry = OPEN_REGISTRY.STRATEGIES[name]
        with pytest.raises(ValueError, match="basket evaluator"):
            entry["fn"](None)
        assert callable(OPEN_REGISTRY.get_basket_ranker(name)["basket_fn"])
        assert name not in shim.STRATEGY_REGISTRY
        with pytest.raises(ValueError, match="single-asset backtest or parity run cannot evaluate it"):
            loader.refuse_basket_evaluator(shim, name)
    with pytest.raises(ValueError, match="not a registered basket evaluator"):
        OPEN_REGISTRY.get_basket_ranker("sma_crossover")


@pytest.mark.parametrize("kwargs, match", [
    ({"basket": {"input": "closed_bar_frames"}}, "basket must be exactly"),
    ({"basket": {"input": "closed_bar_frames", "output": "signed_weights"}, "platforms": ("spot", "futures")}, "platforms"),
    ({"basket": {"input": "closed_bar_frames", "output": "signed_weights"}, "platforms": ("futures",),
      "short_entries": True, "default_params": {"allow_short": True}}, "short_entries"),
    ({"basket_preflight": lambda params, context: None}, "basket_preflight applies only to a basket evaluator"),
    ({"basket": {"input": "closed_bar_frames", "output": "signed_weights"}, "platforms": ("futures",),
      "basket_preflight": "not callable"}, "basket_preflight must be callable"),
])
def test_register_rejects_malformed_basket_metadata(scratch_registry, kwargs, match):
    kwargs = {"default_params": {}, "short_entries": False, **kwargs}
    before = set(scratch_registry.STRATEGIES)
    with pytest.raises(ValueError, match=match):
        scratch_registry.register("_scratch_basket", "scratch", **kwargs)(_noop)
    assert set(scratch_registry.STRATEGIES) == before


def go_pair_policies():
    source = _go_source("pair_config.go")
    consts = dict(re.findall(r'^const (\w+) = "([^"]*)"$', source, re.M))
    lines = re.findall(r"^var pairPolicyStrategies = map\[string\]bool\{(.*)\}$", source, re.M)
    assert len(lines) == 1, "pairPolicyStrategies must be one map literal line"
    raw = [item.strip() for item in lines[0].split(",") if item.strip()]
    names = []
    for item in raw:
        m = re.fullmatch(r'(\w+|"\w+"):\s*true', item)
        assert m, f"unparsed pairPolicyStrategies entry: {item!r}"
        key = m.group(1)
        names.append(key.strip('"') if key.startswith('"') else consts[key])
    assert len(names) == len(raw)
    block = _go_block(source, "var pairPolicyParamKeys = map[string][]string{")
    entries = re.findall(r'^\s*(\w+|"\w+"):\s*\{([^}]*)\},\s*$', block, re.M)
    assert len(entries) == len([line for line in block.splitlines() if line.strip()]), "unparsed pairPolicyParamKeys line"
    keys = {(k.strip('"') if k.startswith('"') else consts[k]): tuple(sorted(re.findall(r'"(\w+)"', v))) for k, v in entries}
    assert set(keys) == set(names)
    return keys


def test_go_pair_policies_equal_python_pair_registry_and_evaluator():
    pairs = OPEN_REGISTRY.pair_registry
    py = {name: OPEN_REGISTRY.STRATEGIES[name]["pair_param_keys"] for name in OPEN_REGISTRY.pair_evaluators()}
    assert set(py) == set(pairs.pair_policies()) == {"cross_exchange_perp_arbitrage"}
    for name in sorted(py):
        descriptor = pairs.pair_descriptor(name)
        entry = OPEN_REGISTRY.STRATEGIES[name]
        assert entry["pair"] == OPEN_REGISTRY.PAIR_CONTRACT == descriptor["contract"]
        assert entry["platforms"] == descriptor["platforms"] == ("futures",)
        assert entry["short_entries"] is False and entry["production_refused"] is True
        assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
            descriptor["edge_status"], descriptor["edge_source"], descriptor["edge_ref"])
        assert entry["default_params"] == {} and entry["constraints"] == () and entry["param_validator"] is None
        evaluate = OPEN_REGISTRY.get_pair_evaluator(name)
        module = sys.modules[descriptor["module"]]
        assert os.path.realpath(module.__file__) == os.path.realpath(os.path.join(REPO, descriptor["path"]))
        assert evaluate is module.evaluate_pair
        assert module.POLICY == name
        assert tuple(module.PARAM_KEYS) == descriptor["param_keys"] == py[name]
    assert go_pair_policies() == {name: tuple(sorted(keys)) for name, keys in py.items()}
    with pytest.raises(OPEN_REGISTRY.UnknownPairPolicy):
        OPEN_REGISTRY.get_pair_evaluator("sma_crossover")


def test_pair_policies_refuse_every_single_symbol_path():
    for platform in OPEN_REGISTRY.VALID_PLATFORMS:
        for hidden in (True, False):
            assert set(OPEN_REGISTRY.build_registry(platform, include_hidden=hidden)).isdisjoint(OPEN_REGISTRY.pair_evaluators())
    loader = _load("_parity_pair_registry_loader", "backtest", "registry_loader.py")
    if os.path.join(REPO, "shared_tools") not in sys.path:
        sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    composition = importlib.import_module("strategy_composition")
    for platform in OPEN_REGISTRY.VALID_PLATFORMS:
        shim = loader.load_registry(platform)
        for name in sorted(OPEN_REGISTRY.pair_evaluators()):
            assert name not in shim.STRATEGY_REGISTRY
            for refuse in (loader.refuse_pair_evaluator, loader.refuse_quote_capability, loader.refuse_basket_evaluator):
                with pytest.raises(ValueError, match="is a pair policy"):
                    refuse(shim, name)
    for name in sorted(OPEN_REGISTRY.pair_evaluators()):
        with pytest.raises(ValueError, match="is a pair policy"):
            OPEN_REGISTRY.STRATEGIES[name]["fn"](None)
        entry = dict(OPEN_REGISTRY.STRATEGIES[name], production_refused=False)
        with pytest.raises(ValueError, match="is a pair policy"):
            composition.admit_strategy_references([composition.StrategyReference("open", name)],
                                                  composition.GateMode(composition.GATE_MODE_PAPER, ""),
                                                  composition.Acknowledgement(True, None), lambda _n: entry)
    for name in ("sma_crossover", "inventory_market_making", "cross_sectional_momentum"):
        loader.refuse_pair_evaluator(loader.load_registry("futures"), name)
        assert name not in OPEN_REGISTRY.pair_evaluators()


@pytest.mark.parametrize("kwargs, match", [
    ({"pair": {"input": "pair_policy_request_v2"}}, "pair must be exactly"),
    ({"basket": {"input": "closed_bar_frames", "output": "signed_weights"}}, "carries no basket metadata"),
    ({"quote": {"input": "deadline_book_evidence"}}, "carries no quote metadata"),
    ({"quote_spec": {"strategy": "cross_exchange_perp_arbitrage"}}, "carries no quote metadata"),
    ({"edge_source": "study_fail"}, "differs from the pair_registry descriptor"),
    ({"edge_ref": "x.md"}, "differs from the pair_registry descriptor"),
    ({"platforms": ("spot", "futures")}, "differs from the pair_registry descriptor"),
    ({"default_params": {"entry_threshold_bps": 5}}, "no single-symbol default_params"),
    ({"param_validator": lambda p: None}, "no single-symbol default_params"),
    ({"name": "_scratch_pair"}, "needs a descriptor"),
])
def test_register_rejects_malformed_pair_metadata(scratch_registry, kwargs, match):
    name = kwargs.pop("name", "cross_exchange_perp_arbitrage")
    descriptor = scratch_registry.pair_registry.pair_descriptor("cross_exchange_perp_arbitrage")
    scratch_registry.STRATEGIES.pop("cross_exchange_perp_arbitrage")
    base = {"description": descriptor["description"], "default_params": {}, "platforms": descriptor["platforms"],
            "edge_status": descriptor["edge_status"], "edge_source": descriptor["edge_source"],
            "edge_ref": descriptor["edge_ref"], "short_entries": False, "production_refused": True,
            "pair": dict(scratch_registry.PAIR_CONTRACT)}
    kwargs = {**base, **kwargs}
    before = dict(scratch_registry.STRATEGIES)
    with pytest.raises(ValueError, match=match):
        scratch_registry.register(name, **kwargs)(_noop)
    assert scratch_registry.STRATEGIES == before
    scratch_registry.register("cross_exchange_perp_arbitrage", **base)(_noop)
    assert scratch_registry.pair_evaluators() == frozenset({"cross_exchange_perp_arbitrage"})
