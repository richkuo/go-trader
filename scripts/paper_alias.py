import re

PAPER_SOURCE_ID = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
RESERVED_PAPER_SOURCE_IDS = ("live", "paper", "primary")


def paper_alias_suffix(source=None):
    sid = (source or "").strip()
    if not sid:
        return "-paper"
    if sid in RESERVED_PAPER_SOURCE_IDS:
        return None
    if not PAPER_SOURCE_ID.match(sid):
        return None
    return "-paper-%s" % sid


def paper_alias_base(sid, source=None):
    suffix = paper_alias_suffix(source)
    if suffix is None:
        return None
    if sid.endswith(suffix):
        return sid[: -len(suffix)] or None
    head, sep, tail = sid.rpartition(suffix)
    if sep and tail.isdigit():
        return head or None
    return None
