import copy
import importlib.util
import os
import re

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
    assert len(py) == 67
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
    }
    out = {}
    for key, ref in re.findall(r'^\s*(\w+|"\w+"):\s*"([^"]*)",\s*$', block, re.M):
        out[key.strip('"') if key.startswith('"') else consts[key]] = ref
    assert len(out) == len([line for line in block.splitlines() if line.strip()]), "unparsed productionRefusedStrategies line"
    return out


def test_go_production_refusal_equals_python_registry():
    go = go_production_refused()
    py = {name: OPEN_REGISTRY.STRATEGIES[name]["edge_ref"] for name in OPEN_REGISTRY.production_refused_strategies()}
    assert py == {"cumulative_volume_delta_divergence": "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md",
                  "bitcoin_lead_lag": "backtest/candidates/bitcoin_lead_lag_1640/REPORT.md"}
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


def refused_in_both_languages():
    return set(go_production_refused()) & set(OPEN_REGISTRY.production_refused_strategies())


def unpromoted_absence(problems, names):
    return [p for p in problems if not any(p == f"{n}: in Python, missing from Go" for n in names)]


def test_go_bidirectional_perps_equal_python_short_entries():
    go, py = go_bidirectional_perps(), python_short_entries()
    assert py
    assert unpromoted_absence(mapping_mismatches(go, py), refused_in_both_languages()) == []
    assert "bitcoin_lead_lag" in py and "bitcoin_lead_lag" not in go


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
