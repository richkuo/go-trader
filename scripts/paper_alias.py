def paper_alias_base(sid):
    if sid.endswith("-paper"):
        return sid[: -len("-paper")]
    head, sep, tail = sid.rpartition("-paper")
    if sep and tail.isdigit():
        return head
    return None
