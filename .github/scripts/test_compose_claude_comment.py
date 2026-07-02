"""Unit tests for compose_claude_comment.py.

Run: python3 .github/scripts/test_compose_claude_comment.py
"""

import os
import sys
import unittest
import urllib.parse

sys.path.insert(0, os.path.dirname(__file__))
from compose_claude_comment import compose, model_display_name

HARNESS = "anthropics/claude-code-action@v1"


class ModelDisplayNameTest(unittest.TestCase):
    def test_known_ids_mapped(self):
        self.assertEqual(model_display_name("claude-opus-4-8[1m]"), "Claude Opus 4.8 (1M)")
        self.assertEqual(model_display_name("claude-sonnet-5"), "Claude Sonnet 5")
        self.assertEqual(model_display_name("claude-fable-5"), "Claude Fable 5")

    def test_unknown_id_passes_through(self):
        self.assertEqual(model_display_name("claude-next-6"), "claude-next-6")

    def test_empty_id_marks_unresolved(self):
        self.assertEqual(model_display_name(""), "(model not resolved)")


class ComposeTest(unittest.TestCase):
    def test_appends_footer(self):
        out = compose("## Review\nLGTM", "claude-sonnet-5", "xhigh", HARNESS)
        self.assertEqual(
            out,
            "## Review\nLGTM\n\n---\nLLM: Claude Sonnet 5 | xhigh | Harness: " + HARNESS,
        )

    def test_empty_effort_reads_unknown(self):
        out = compose("body", "claude-sonnet-5", "", HARNESS)
        self.assertTrue(
            out.endswith("LLM: Claude Sonnet 5 | unknown | Harness: " + HARNESS)
        )

    def test_replaces_stale_footer(self):
        once = compose("body", "claude-sonnet-5", "medium", HARNESS)
        twice = compose(once, "claude-fable-5", "xhigh", HARNESS)
        self.assertEqual(twice.count("LLM:"), 1)
        self.assertIn("Claude Fable 5 | xhigh", twice)
        self.assertNotIn("medium", twice)

    def test_status_note_before_footer(self):
        note = "**Workflow failed before completion.** See [run log](http://x)."
        out = compose("body", "", "", HARNESS, note)
        self.assertIn("body\n\n**Workflow failed before completion.**", out)
        self.assertTrue(
            out.endswith("LLM: (model not resolved) | unknown | Harness: " + HARNESS)
        )

    def test_empty_body_composes_standalone_status_comment(self):
        # patch_claude_comment.sh ON_MISS=post composes a NEW comment from an
        # empty body — no leading blank lines before the status note.
        note = "**Workflow failed before completion.** See [run log](http://x)."
        out = compose("", "claude-sonnet-5", "high", HARNESS, note)
        self.assertTrue(out.startswith("**Workflow failed before completion.**"))
        self.assertIn("LLM: Claude Sonnet 5 | high", out)

    def test_stale_status_note_stripped_on_success_retry(self):
        note = "**Workflow cancelled before completion.** See [run log](http://x)."
        failed = compose("body", "claude-sonnet-5", "high", HARNESS, note)
        retried = compose(failed, "claude-sonnet-5", "high", HARNESS)
        self.assertNotIn("before completion", retried)
        self.assertEqual(retried.count("LLM:"), 1)

    def test_cancel_then_fail_replaces_note(self):
        cancelled = "**Workflow cancelled before completion.** See [run log](http://x)."
        failed = "**Workflow failed before completion.** See [run log](http://y)."
        first = compose("body", "claude-sonnet-5", "high", HARNESS, cancelled)
        second = compose(first, "claude-sonnet-5", "high", HARNESS, failed)
        self.assertNotIn("cancelled", second)
        self.assertEqual(second.count("before completion"), 1)

    def test_idempotent(self):
        note = "**Workflow failed before completion.** See [run log](http://x)."
        once = compose("body", "claude-sonnet-5", "high", HARNESS, note)
        twice = compose(once, "claude-sonnet-5", "high", HARNESS, note)
        self.assertEqual(once, twice)

    def test_rewrites_create_pr_link(self):
        pr_body = "## Summary\n\nGenerated with [Claude Code](https://claude.com/claude-code)"
        encoded = urllib.parse.quote(pr_body, safe="")
        comment = (
            "[Create PR](https://github.com/o/r/compare/main...b"
            f"?quick_pull=1&body={encoded})"
        )
        out = compose(comment, "claude-sonnet-5", "xhigh", HARNESS)
        decoded = urllib.parse.unquote(out)
        self.assertNotIn("claude.com/claude-code", decoded)
        # One footer inside the rewritten link body, one on the comment itself.
        self.assertEqual(decoded.count("LLM:"), 2)


class ComposeCLITest(unittest.TestCase):
    def test_cli_matches_function(self):
        import subprocess

        script = os.path.join(os.path.dirname(__file__), "compose_claude_comment.py")
        env = {
            **os.environ,
            "BODY_IN": "body",
            "MODEL_ID": "claude-opus-4-8[1m]",
            "EFFORT": "xhigh",
            "CLAUDE_HARNESS": HARNESS,
            "STATUS_NOTE": "",
        }
        result = subprocess.run(
            [sys.executable, script], env=env, capture_output=True, text=True, check=True
        )
        self.assertEqual(
            result.stdout, compose("body", "claude-opus-4-8[1m]", "xhigh", HARNESS)
        )


if __name__ == "__main__":
    unittest.main()
