from typing import Optional

OPEN_INTEREST = "open_interest"
TRADE_FLOW = "trade_flow"
L2_BOOK = "l2_book"
PERP_PREMIUM = "perp_premium"
V3_KINDS = (OPEN_INTEREST, TRADE_FLOW, L2_BOOK, PERP_PREMIUM)
V3_TYPED_CONTRACTS = ((L2_BOOK, "book_contract"), (PERP_PREMIUM, "premium_contract"))
BOOK_RECORD_KINDS = ("book", "book_sub", "book_marker", "book_need", "book_release")
PREMIUM_RECORD_KINDS = ("bbo", "pctx", "pbad", "ptick", "psample", "pinvalidate")


def v3_kinds_problem(doc) -> Optional[str]:
    if not isinstance(doc, dict):
        return "is not a JSON object"
    kinds = doc.get("kinds")
    if not isinstance(kinds, list) or not kinds or any(not isinstance(k, str) for k in kinds):
        return f"lists no kinds ({kinds!r})"
    if len(set(kinds)) != len(kinds):
        return f"lists a kind more than once ({kinds!r})"
    unknown = sorted(set(kinds) - set(V3_KINDS))
    if unknown:
        return f"names unknown kind(s) {unknown}; recording schema v3 knows only {list(V3_KINDS)}"
    for kind, block in V3_TYPED_CONTRACTS:
        if (kind in kinds) != (block in doc):
            if kind in kinds:
                return f"records {kind} but carries no {block}"
            return f"carries {block} but does not record {kind}"
    return None


def v3_other_kind_records(kinds) -> frozenset:
    out = set()
    if L2_BOOK in kinds:
        out.update(BOOK_RECORD_KINDS)
    if PERP_PREMIUM in kinds:
        out.update(PREMIUM_RECORD_KINDS)
    return frozenset(out)
