import hashlib
import os


def okx_account_key(inst_type):
    api_key = os.environ.get("OKX_API_KEY", "").strip()
    if not api_key:
        return ""
    network = "sandbox" if os.environ.get("OKX_SANDBOX", "") == "1" else "live"
    fingerprint = hashlib.sha256(("go-trader/okx-api-key/v1:" + api_key).encode()).hexdigest()[:16]
    return f"okx:{inst_type}:{network}:key-{fingerprint}"
