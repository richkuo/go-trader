
import os
import subprocess
import sys
import unittest
import urllib.parse

SCRIPT = os.path.join(os.path.dirname(__file__), "rewrite_create_pr_link.py")
FOOTER = "---\nLLM: Claude Opus 4.7 (1M) | high"


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

    def test_replaces_claude_ai_code_attribution(self):
        default_attr = "Generated with [Claude Code](https://claude.ai/code)"
        pr_body = f"## Summary\n- did a thing\n\n{default_attr}"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"[Create PR](https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"

        out = run(comment)
        new_body = extract_body_param(out)

        self.assertNotIn("claude.ai/code", new_body)
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
        self.assertEqual(new_body.count("LLM:"), 1)

    def test_no_link_leaves_body_unchanged(self):
        comment = "Just a plain comment with no Create PR link."
        self.assertEqual(run(comment), comment)

    def test_rewrites_multiple_links(self):
        pr_body = "## Summary"
        encoded = urllib.parse.quote(pr_body, safe="")
        link = f"(https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"
        comment = f"first {link} and second {link}"

        out = run(comment)
        self.assertEqual(urllib.parse.unquote(out).count("LLM:"), 2)

    def test_replaces_stale_llm_footer_in_prefilled_body(self):
        stale_footer = "---\nLLM: Claude Opus 4.7 (1M) | medium"
        pr_body = f"## Summary\n- did a thing\n\n{stale_footer}"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"[Create PR](https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"

        out = run(comment)
        new_body = extract_body_param(out)

        self.assertTrue(new_body.endswith(FOOTER), msg=f"Body did not end with footer:\n{new_body}")
        self.assertEqual(new_body.count("LLM:"), 1)
        self.assertIn("## Summary", new_body)

    def test_idempotent(self):
        pr_body = "## Summary"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = f"[x](https://github.com/owner/repo/compare/main...feat?quick_pull=1&body={encoded})"

        once = run(comment)
        twice = run(once)

        self.assertEqual(
            extract_body_param(once),
            extract_body_param(twice),
        )


if __name__ == "__main__":
    unittest.main()
