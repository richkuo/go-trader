import os
import sys

_LEDGER_URL = os.environ.get("HL_REQUEST_LEDGER_URL", "").strip()

if _LEDGER_URL:
    if os.environ.get("HYPERLIQUID_SECRET_KEY", "").strip():
        print("[hl-request-ledger] HYPERLIQUID_SECRET_KEY is set; this process is not redirected (measure paper copies without credentials)",
              file=sys.stderr)
    else:
        try:
            import hyperliquid.api as _hl_api
        except ImportError:
            _hl_api = None
        if _hl_api is not None:
            _original_init = _hl_api.API.__init__

            def _ledger_init(self, base_url=None, timeout=None):
                if any(cls.__name__ == "Exchange" for cls in type(self).__mro__):
                    _original_init(self, base_url, timeout)
                    return
                _original_init(self, _LEDGER_URL, timeout)

            _hl_api.API.__init__ = _ledger_init
