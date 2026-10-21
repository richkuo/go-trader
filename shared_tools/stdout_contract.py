import contextlib
import json
import sys

_kept_stdout = None


def _emitter(handle):
    def emit(payload):
        handle.write(json.dumps(payload) + "\n")
        handle.flush()

    return emit


@contextlib.contextmanager
def json_stdout():
    global _kept_stdout
    if _kept_stdout is not None:
        yield _emitter(_kept_stdout)
        return
    kept = sys.stdout
    _kept_stdout = kept
    sys.stdout = sys.stderr
    try:
        yield _emitter(kept)
    finally:
        sys.stdout = kept
        _kept_stdout = None
