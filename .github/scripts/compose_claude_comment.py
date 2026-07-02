"""Compose the authoritative body for a claude[bot] comment.

Single source of truth for the model-id display names, the LLM footer, and
the workflow status note that both claude.yml patch steps (success footer and
cancellation/failure note) append. As a CLI, reads the current comment body
from $BODY_IN plus $MODEL_ID, $EFFORT, $CLAUDE_HARNESS, and optional
$STATUS_NOTE; prints the recomposed body to stdout.

Idempotent: a stale LLM footer (Claude guesses the effort; the workflow is
authoritative) and any previous workflow status note are stripped before the
fresh note and footer are appended, so retries and repeated failures never
stack. The "Create PR" link's prefilled body= param is rewritten to end with
the same footer (see rewrite_create_pr_link.py).
"""

import os
import re
import sys

from rewrite_create_pr_link import rewrite_create_pr_link
from strip_llm_footer import strip_llm_footer

MODEL_DISPLAY_NAMES = {
    "claude-opus-4-8[1m]": "Claude Opus 4.8 (1M)",
    "claude-sonnet-5": "Claude Sonnet 5",
    "claude-fable-5": "Claude Fable 5",
}

_STATUS_NOTE = re.compile(
    r"\n*\*\*Workflow (?:cancelled|failed) before completion\.\*\*[^\n]*\n?"
)


def model_display_name(model_id: str) -> str:
    if not model_id:
        return "(model not resolved)"
    return MODEL_DISPLAY_NAMES.get(model_id, model_id)


def compose(
    body: str, model_id: str, effort: str, harness: str, status_note: str = ""
) -> str:
    body = strip_llm_footer(body)
    body = _STATUS_NOTE.sub("", body).rstrip()
    footer = (
        f"---\nLLM: {model_display_name(model_id)}"
        f" | {effort or 'unknown'} | Harness: {harness}"
    )
    # body is empty when composing a standalone status comment (ON_MISS=post
    # in patch_claude_comment.sh) — omit it so the note leads the comment.
    parts = [body] if body else []
    if status_note:
        parts.append(status_note)
    parts.append(footer)
    return rewrite_create_pr_link("\n\n".join(parts), footer)


if __name__ == "__main__":
    sys.stdout.write(
        compose(
            os.environ["BODY_IN"],
            os.environ.get("MODEL_ID", ""),
            os.environ.get("EFFORT", ""),
            os.environ["CLAUDE_HARNESS"],
            os.environ.get("STATUS_NOTE", ""),
        )
    )
