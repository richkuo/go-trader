"""Rewrite the "Create PR" link inside a claude[bot] comment so the prefilled
PR body ends with our model/effort footer instead of the default
"Generated with [Claude Code](...)" attribution.

Reads the comment body from $BODY_IN and the footer text from $FOOTER_TEXT.
Prints the rewritten comment body to stdout.

Not idempotent on its own: if a Create-PR link's `body=` already ends with a
`LLM: ...` footer (not the default Claude Code attribution), the
else-branch in `rewrite` will append a second footer. The caller in
.github/workflows/claude.yml guards against re-runs with a
`grep -q "LLM:"` check before invoking this script.
"""

import os
import re
import sys
import urllib.parse

body = os.environ["BODY_IN"]
footer = os.environ["FOOTER_TEXT"]

default_attr = re.compile(
    r"\n*Generated with \[Claude Code\]\(https://claude\.(?:com/claude-code|ai/code)\)\s*\Z"
)


def rewrite(match):
    url = match.group(1)
    parts = urllib.parse.urlsplit(url)
    qs = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    new_qs = []
    for k, v in qs:
        if k == "body":
            if default_attr.search(v):
                v = default_attr.sub("\n\n" + footer, v)
            else:
                v = v.rstrip() + "\n\n" + footer
        new_qs.append((k, v))
    new_query = urllib.parse.urlencode(new_qs, safe="", quote_via=urllib.parse.quote)
    new_url = urllib.parse.urlunsplit(
        (parts.scheme, parts.netloc, parts.path, new_query, parts.fragment)
    )
    return "(" + new_url + ")"


body = re.sub(
    r"\((https://github\.com/[^)\s]*compare/[^)\s]*[?&]quick_pull=1[^)\s]*)\)",
    rewrite,
    body,
)
sys.stdout.write(body)
sys.stdout.write("\n")
