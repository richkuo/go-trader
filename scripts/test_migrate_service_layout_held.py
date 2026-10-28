"""The migrate plan names a hold only when it is the strategy's latest evidence."""

import importlib.util
from pathlib import Path

_path = Path(__file__).resolve().parent / "migrate-service-layout.py"
_spec = importlib.util.spec_from_file_location("migrate_service_layout", _path)
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)


def _rev(reason, hold_at, evaluated_at, sid="A"):
    return {
        "held": {sid: {"reason": reason, "at": hold_at}},
        "evaluated": {sid: evaluated_at},
    }


def test_a_later_evaluation_drops_a_kill_switch_hold():
    got = mod.held_by_latest_cycle(_rev("portfolio_kill_switch", "2026-10-27T10:00:00Z", "2026-10-27T10:00:01Z"))
    assert got.get("portfolio_kill_switch", []) == []


def test_a_later_kill_switch_hold_stays_in_the_note():
    got = mod.held_by_latest_cycle(_rev("portfolio_kill_switch", "2026-10-27T10:00:01Z", "2026-10-27T10:00:00Z"))
    assert got.get("portfolio_kill_switch") == ["A"]


def test_a_later_evaluation_drops_a_save_blocked_hold():
    got = mod.held_by_latest_cycle(_rev("save_blocked", "2026-10-27T10:00:00Z", "2026-10-27T10:00:01Z", sid="B"))
    assert got == {}
