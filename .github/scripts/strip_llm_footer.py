"""Strip a trailing LLM footer from a comment body.

Reads from stdin, writes the stripped body to stdout.  Idempotent: a body
with no LLM footer passes through unchanged.

Stripped pattern (anchored to end-of-string):
    <newlines> [--- <newlines>] LLM: <rest-of-line> <trailing-whitespace>

The pattern is anchored to end-of-string (\\Z) so an "LLM:" token elsewhere in the body
(e.g. inside a code block) is preserved.
"""

import re
import sys

_LLM_FOOTER = re.compile(
    r"\n+(?:---\n+)?LLM:[^\n]*\s*\Z",
    re.MULTILINE,
)


def strip_llm_footer(body: str) -> str:
    return _LLM_FOOTER.sub("", body)


if __name__ == "__main__":
    body = sys.stdin.read()
    sys.stdout.write(strip_llm_footer(body))
