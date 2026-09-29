import os

_LEDGER_URL = os.environ.get("HL_REQUEST_LEDGER_URL", "").strip()

if _LEDGER_URL:
    try:
        import hyperliquid.api as _hl_api
    except ImportError:
        _hl_api = None
    if _hl_api is not None:
        _original_init = _hl_api.API.__init__

        def _ledger_init(self, base_url=None, timeout=None):
            _original_init(self, _LEDGER_URL, timeout)

        _hl_api.API.__init__ = _ledger_init
