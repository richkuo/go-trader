import pathlib
import re
import sys

SCRIPTS = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))

from paper_alias import paper_alias_base

DEFINITION = re.compile(r"^\s*def (?:paper_alias_base|strip_paper_suffix)\(", re.M)
CONSUMER = re.compile(r"^from paper_alias import paper_alias_base$", re.M)


def scripts_matching(pattern):
    hits = []
    for path in sorted(SCRIPTS.iterdir()):
        if not path.is_file() or path.name == pathlib.Path(__file__).name:
            continue
        if pattern.search(path.read_text(errors="ignore")):
            hits.append(path.name)
    return hits


def test_paper_alias_base_reads_the_suffix():
    cases = [
        ("hl-rsi-btc-60-paper", "hl-rsi-btc-60"),
        ("hl-rsi-btc-60-paper2", "hl-rsi-btc-60"),
        ("hl-rsi-btc-60-paper10", "hl-rsi-btc-60"),
        ("hl-rsi-btc-60", None),
        ("hl-rsi-btc-60-paperx", None),
        ("hl-paper-rsi", None),
        ("hl-rsi-btc-60-paper-paper", "hl-rsi-btc-60-paper"),
    ]
    got = [(sid, paper_alias_base(sid)) for sid, _ in cases]
    assert got == cases


def test_the_suffix_rule_is_defined_once():
    assert scripts_matching(DEFINITION) == ["paper_alias.py"]


def test_both_paper_tools_load_the_shared_rule():
    assert scripts_matching(CONSUMER) == [
        "check-live-paper-config-drift.sh",
        "merge-paper-instance.sh",
    ]
