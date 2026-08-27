import os
import re
import sys
from rewrite_create_pr_link import rewrite_create_pr_link
from strip_llm_footer import strip_llm_footer
MODEL_DISPLAY_NAMES = {'claude-opus-4-8[1m]': 'Claude Opus 4.8 (1M)', 'claude-sonnet-5': 'Claude Sonnet 5', 'claude-fable-5': 'Claude Fable 5'}
_STATUS_NOTE = re.compile('\\n*\\*\\*Workflow (?:cancelled|failed) before completion\\.\\*\\*[^\\n]*\\n?')

def model_display_name(model_id: str) -> str:
    if not model_id:
        return '(model not resolved)'
    return MODEL_DISPLAY_NAMES.get(model_id, model_id)

def compose(body: str, model_id: str, effort: str, harness: str, status_note: str='') -> str:
    body = strip_llm_footer(body)
    body = _STATUS_NOTE.sub('', body).rstrip()
    footer = f"---\nLLM: {model_display_name(model_id)} | {effort or 'unknown'} | Harness: {harness}"
    parts = [body] if body else []
    if status_note:
        parts.append(status_note)
    parts.append(footer)
    return rewrite_create_pr_link('\n\n'.join(parts), footer)
if __name__ == '__main__':
    sys.stdout.write(compose(os.environ['BODY_IN'], os.environ.get('MODEL_ID', ''), os.environ.get('EFFORT', ''), os.environ['CLAUDE_HARNESS'], os.environ.get('STATUS_NOTE', '')))
