"""Unit tests for rewrite_create_pr_link.py.

Invokes the script as a subprocess with BODY_IN/FOOTER_TEXT env vars and
asserts the rewritten stdout — covers the default-attribution replacement,
fallback append, no-link no-op, and multi-link cases.

Run: python3 .github/scripts/test_rewrite_create_pr_link.py
"""

import os
import subprocess
import sys
import unittest
import urllib.parse

SCRIPT = os.path.join(os.path.dirname(__file__), "rewrite_create_pr_link.py")
FOOTER = "---\nGenerated with: Claude Opus 4.7 (1M) | Effort: high"


def run(body_in, footer=FOOTER):
    result = subprocess.run(
        [sys.executable, SCRIPT],
        env={**os.environ, "BODY_IN": body_in, "FOOTER_TEXT": footer},
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout.rstrip("\n")


def extract_body_param(markdown):
    """Pull the `body=` query param out of the first Create-PR link."""
    start = markdown.index("(https://github.com/")
    end = markdown.index(")", start)
    url = markdown[start + 1 : end]
    qs = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query, keep_blank_values=True)
    return qs["body"][0]


class RewriteCreatePRLinkTest(unittest.TestCase):
    def test_replaces_default_attribution(self):
        default_attr = "Generated with [Claude Code](https://claude.com/claude-code)"
        pr_body = f"## Summary\n- did a thing\n\n{default_attr}"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"See [Create PR ➔](https://github.com/owner/repo/compare/main...feat?quick_pull=1&title=x&body={encoded})"

        out = run(comment)
        new_body = extract_body_param(out)

        self.assertNotIn("claude.com/claude-code", new_body)
        self.assertTrue(new_body.endswith(FOOTER))
        self.assertIn("## Summary", new_body)

    def test_appends_when_no_default_attribution(self):
        pr_body = "## Summary\n- did a thing"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"[Create PR](https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"

        out = run(comment)
        new_body = extract_body_param(out)

        self.assertTrue(new_body.endswith(FOOTER))
        self.assertIn("## Summary", new_body)
        self.assertEqual(new_body.count("Generated with:"), 1)

    def test_no_link_leaves_body_unchanged(self):
        comment = "Just a plain comment with no Create PR link."
        self.assertEqual(run(comment), comment)

    def test_rewrites_multiple_links(self):
        pr_body = "## Summary"
        encoded = urllib.parse.quote(pr_body, safe="")
        link = f"(https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"
        comment = f"first {link} and second {link}"

        out = run(comment)
        # `Generated with:` appears URL-encoded inside the `body=` param —
        # decode the whole output to count occurrences across both links.
        self.assertEqual(urllib.parse.unquote(out).count("Generated with:"), 2)

    def test_non_idempotent_without_shell_guard(self):
        """Documents the idempotency caveat: running twice appends twice.
        The shell guard in claude.yml (grep -q 'Generated with:') is what
        prevents this in production."""
        pr_body = "## Summary"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"[x](https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"

        once = run(comment)
        twice = run(once)

        self.assertEqual(urllib.parse.unquote(once).count("Generated with:"), 1)
        self.assertEqual(urllib.parse.unquote(twice).count("Generated with:"), 2)


if __name__ == "__main__":
    unittest.main()
