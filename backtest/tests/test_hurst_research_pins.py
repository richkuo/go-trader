import importlib
import os
import sys

_HERE = os.path.dirname(__file__)
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_RESEARCH = os.path.abspath(os.path.join(_HERE, "..", "research"))

sys.path.insert(0, _RESEARCH)
sys.path.insert(0, os.path.abspath(os.path.join(_HERE, "..")))

GO_GATE = os.path.join(_REPO, "scheduler", "hurst_gate.go")


def _study(name):
    return importlib.import_module(name)


# --- the shipped gate the runtime actually reads -----------------------------


def test_the_go_gate_still_implements_the_form_the_parity_module_scores():
    study = _study("hurst_1428_sizing_exit")
    src = open(GO_GATE).read()
    assert f"hurstSizeSpan = {study.SHIPPED_SIZE_SPAN}" in src
    assert f"hurstDefaultSizeFloor = {study.SHIPPED_SIZE_FLOOR}" in src
    body = src.split("func hurstSizeMultiplier(", 1)[1].split("\n}", 1)[0]
    assert "math.Abs(h-0.5) / hurstSizeSpan" in body
    assert "m > 1.0" in body and "m = 1.0" in body
    assert "m < floor" in body and "m = floor" in body
    assert "return 1.0" in body
