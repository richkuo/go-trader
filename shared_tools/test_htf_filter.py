
from shared_tools.conftest import load_module

_HTF_FILTER = load_module("_htf_filter_test", __file__.replace("test_htf_filter.py", "htf_filter.py"))
get_default_htf = _HTF_FILTER.get_default_htf


def test_htf_map_matches_the_shared_fixture():
    import json
    import os

    fixture = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "htf_map.json")
    with open(fixture) as fh:
        spec = json.load(fh)
    assert _HTF_FILTER._HTF_MAP == spec["map"]
    assert get_default_htf("7m") == spec["default"]
    assert spec["lookback"] == 50 + 10
