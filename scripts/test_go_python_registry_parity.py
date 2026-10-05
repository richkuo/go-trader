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
    assert len(py) == 42
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


def test_go_registered_platforms_equal_python_registry():
    go, py = go_registered_platforms(), python_registered_platforms()
    assert len(py) == len(OPEN_REGISTRY.STRATEGIES)
    assert mapping_mismatches(go, py) == []


def test_every_registered_open_strategy_has_a_unique_short_name():
    short = go_short_names()
    assert set(OPEN_REGISTRY.STRATEGIES) - set(short) == set()
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
