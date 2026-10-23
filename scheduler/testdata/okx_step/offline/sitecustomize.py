import builtins
import json
import os
import socket

_LOG = os.environ.get("OKX_STEP_OFFLINE_LOG", "")


def _record(kind, what):
    if _LOG:
        with open(_LOG, "a") as fh:
            fh.write(json.dumps([kind, str(what)]) + "\n")


def _blocked_connect(self, address, *args, **kwargs):
    _record("connect", address)
    raise OSError("network blocked by the okx step offline fixture: %r" % (address,))


socket.socket.connect = _blocked_connect
socket.socket.connect_ex = _blocked_connect

_real_import = builtins.__import__


def _guard(name, *args, **kwargs):
    if name in ("adapter", "ccxt") or name.startswith("ccxt."):
        _record("import", name)
        raise ImportError("frozen management input must not construct an exchange client: " + name)
    return _real_import(name, *args, **kwargs)


builtins.__import__ = _guard
