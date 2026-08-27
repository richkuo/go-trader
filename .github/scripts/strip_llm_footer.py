import re
import sys
_LLM_FOOTER = re.compile('\\n+(?:---\\n+)?LLM:[^\\n]*\\s*\\Z', re.MULTILINE)

def strip_llm_footer(body: str) -> str:
    return _LLM_FOOTER.sub('', body)
if __name__ == '__main__':
    body = sys.stdin.read()
    sys.stdout.write(strip_llm_footer(body))
