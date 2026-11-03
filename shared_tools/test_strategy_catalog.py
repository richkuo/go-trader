import importlib.util
import json
import os
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CATALOG = os.path.join(REPO, "shared_tools", "strategy_catalog.py")


def _load(module_name, *parts):
    spec = importlib.util.spec_from_file_location(module_name, os.path.join(REPO, *parts))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _catalog(*args):
    proc = subprocess.run([sys.executable, CATALOG, "--list-json", *args], capture_output=True, text=True, cwd=REPO,
                          timeout=240)
    return proc.returncode, json.loads(proc.stdout.strip().splitlines()[-1])


def test_catalog_lists_every_canonical_open_and_native_close_with_role_route_and_hold_metadata():
    opens = _load("_catalog_test_open_registry", "shared_strategies", "open", "registry.py")
    closes = _load("_catalog_test_close_registry", "shared_strategies", "close", "registry.py")
    code, out = _catalog()
    assert code == 0 and out["schema_version"] == 1
    entries = out["entries"]
    assert [(e["kind"], e["id"]) for e in entries] == sorted((e["kind"], e["id"]) for e in entries)
    assert {e["id"] for e in entries if e["kind"] == "open"} == set(opens.STRATEGIES)
    assert {e["id"] for e in entries if e["kind"] == "close"} == set(closes.STRATEGIES)
    assert not {"cross_exchange_perp_arbitrage", "spread_pair", "basket"} & {e["id"] for e in entries}
    by_key = {(e["kind"], e["id"]): e for e in entries}
    research = opens.production_refused_strategies()
    for name, entry in opens.STRATEGIES.items():
        row = by_key[("open", name)]
        assert row["platforms"] == list(entry["platforms"]) and row["short_entries"] is entry["short_entries"]
        assert (row["edge_status"], row["edge_source"], row["edge_ref"]) == (
            entry["edge_status"], entry["edge_source"], entry["edge_ref"])
        assert row["production_refused"] is (name in research)
        paper = row["paper_execution"]
        if entry.get("quote"):
            assert row["role"] == "quote_capability" and row["owner_role"] == "quote_coordinator"
            assert paper["status"] == "held" and paper["reason"].startswith(opens.QUOTE_ENGINE_HOLD["reason"])
            assert paper["tracking_url"] == opens.QUOTE_ENGINE_HOLD["tracking_url"]
            assert row["live_execution"] == "refused"
        elif name in research:
            assert row["role"] == "scalar_open" and paper["status"] == "available"
            assert paper["route"] == opens.RESEARCH_PAPER_ROUTE and row["live_execution"] == "refused"
            assert any("sealed inputs attach only to the open strategy" in r for r in paper["requires"])
        else:
            assert row["role"] == "scalar_open" and paper["status"] == "available" and paper["route"] is None
            assert row["live_execution"] == ("acknowledgement_required" if entry["edge_status"] else "allowed")
    for name, entry in closes.STRATEGIES.items():
        row = by_key[("close", name)]
        assert row["role"] == "native_close" and "edge_status" not in row
        assert row["research_only"] is entry["research_only"]
        assert row["paper_execution"]["status"] == ("held" if entry["research_only"] else "available")
        assert row["owner_role"] == ("quote_coordinator" if entry["research_only"] else "scalar")
    for platform in ("spot", "futures"):
        code, filtered = _catalog("--platform", platform)
        assert code == 0
        assert {(e["kind"], e["id"]) for e in filtered["entries"]} == {
            key for key, e in by_key.items() if platform in e["platforms"]}
    code, only_close = _catalog("--kind", "close")
    assert code == 0 and {e["kind"] for e in only_close["entries"]} == {"close"}
    code, bad = _catalog("--kind", "basket")
    assert code == 1 and "--kind" in bad["error"]
    shim = subprocess.run([sys.executable, os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py"),
                           "--list-json"], capture_output=True, text=True, cwd=REPO, timeout=240, check=True)
    rows = json.loads(shim.stdout)
    assert rows and all(set(r) == {"id", "description"} for r in rows)
    assert not research & {r["id"] for r in rows}
