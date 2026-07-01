"""Rewrite the "Create PR" link inside a claude[bot] comment so the prefilled
PR body ends with our model/effort footer instead of the default
"Generated with [Claude Code](...)" attribution.

As a CLI, reads the comment body from $BODY_IN and the footer text from
$FOOTER_TEXT and prints the rewritten comment body to stdout. Importable as
rewrite_create_pr_link(body, footer) for callers that already hold both
(compose_claude_comment.py).

Idempotent: any existing LLM footer in the prefilled body= param is stripped
before the authoritative footer is appended (via strip_llm_footer.strip_llm_footer).
"""

import os
import re
import sys
import urllib.parse

from strip_llm_footer import strip_llm_footer

_DEFAULT_ATTR = re.compile(
    r"\n*Generated with \[Claude Code\]\(https://claude\.(?:com/claude-code|ai/code)\)\s*\Z"
)

_CREATE_PR_LINK = re.compile(
    r"\((https://github\.com/[^)\s]*compare/[^)\s]*[?&]quick_pull=1[^)\s]*)\)"
)


def rewrite_create_pr_link(body: str, footer: str) -> str:
    def rewrite(match):
        url = match.group(1)
        parts = urllib.parse.urlsplit(url)
        qs = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
        new_qs = []
        for k, v in qs:
            if k == "body":
                v = strip_llm_footer(v)
                if _DEFAULT_ATTR.search(v):
                    v = _DEFAULT_ATTR.sub("\n\n" + footer, v)
                else:
                    v = v.rstrip() + "\n\n" + footer
            new_qs.append((k, v))
        new_query = urllib.parse.urlencode(new_qs, safe="", quote_via=urllib.parse.quote)
        new_url = urllib.parse.urlunsplit(
            (parts.scheme, parts.netloc, parts.path, new_query, parts.fragment)
        )
        return "(" + new_url + ")"

    return _CREATE_PR_LINK.sub(rewrite, body)


if __name__ == "__main__":
    sys.stdout.write(
        rewrite_create_pr_link(os.environ["BODY_IN"], os.environ["FOOTER_TEXT"])
    )
    sys.stdout.write("\n")
