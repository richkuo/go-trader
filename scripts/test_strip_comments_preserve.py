import importlib.util
from pathlib import Path

_SCRIPT = Path(__file__).resolve().parent / "strip_comments_preserve.py"
_SPEC = importlib.util.spec_from_file_location("strip_comments_preserve", _SCRIPT)
strip = importlib.util.module_from_spec(_SPEC)
assert _SPEC.loader is not None
_SPEC.loader.exec_module(strip)


def test_iter_source_files_skips_lightweight_charts_production_bundle():
    files = strip.iter_source_files()
    assert not any(
        path.endswith("lightweight-charts.standalone.production.js") for path in files
    )
