"""Unit tests for strip_llm_footer.py.

Run: python3 .github/scripts/test_strip_llm_footer.py
"""

import sys
import os
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from strip_llm_footer import strip_llm_footer


class StripLLMFooterTest(unittest.TestCase):
    def test_strips_footer_with_separator(self):
        body = "## Summary\n- did a thing\n\n---\nLLM: Claude Opus 4.7 (1M) | medium"
        self.assertEqual(strip_llm_footer(body), "## Summary\n- did a thing")

    def test_strips_footer_without_separator(self):
        body = "## Summary\n- did a thing\n\nLLM: Claude Opus 4.7 (1M) | high"
        self.assertEqual(strip_llm_footer(body), "## Summary\n- did a thing")

    def test_strips_footer_with_trailing_whitespace(self):
        body = "## Summary\n\n---\nLLM: Claude Opus 4.7 (1M) | high\n\n"
        self.assertEqual(strip_llm_footer(body), "## Summary")

    def test_passthrough_when_no_footer(self):
        body = "## Summary\n- did a thing"
        self.assertEqual(strip_llm_footer(body), body)

    def test_passthrough_empty(self):
        self.assertEqual(strip_llm_footer(""), "")

    def test_does_not_strip_llm_mid_body(self):
        body = "Use LLM: Claude for this.\n\n## Summary\n- details"
        self.assertEqual(strip_llm_footer(body), body)

    def test_idempotent(self):
        body = "## Summary\n\n---\nLLM: Claude Opus 4.7 (1M) | high"
        stripped = strip_llm_footer(body)
        self.assertEqual(strip_llm_footer(stripped), stripped)

    def test_multiple_newlines_before_footer(self):
        body = "## Summary\n\n\n\n---\nLLM: Claude Opus 4.7 (1M) | medium"
        self.assertEqual(strip_llm_footer(body), "## Summary")


if __name__ == "__main__":
    unittest.main()
