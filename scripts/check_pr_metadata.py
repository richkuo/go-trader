#!/usr/bin/env python3
import os
import re
import subprocess
import sys

TITLE_RE = re.compile(r"^[a-z]+(\([^)]*\))?: .+ \[C[0-9]+, [^,]+, [a-z]+(, (plan|fableplan))?\]$")
TITLE_SCOPE_RE = re.compile(r"^[a-z]+\(([^)]*)\)")
CLOSING_RE = re.compile(r"(?<![\w/])(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#([0-9]+)\b", re.IGNORECASE)
INLINE_CODE_RE = re.compile(r"`[^`]*`")
FOOTER_LINE_RE = re.compile(r"^(?:[A-Z][a-z]+ with )?LLM: [^|\s][^|]* \| [^|\s][^|]* \| Harness: [^|\s][^|]*$")
PR_TRAILER_RE = re.compile(r"^https://claude\.ai/code/session_[A-Za-z0-9]+$")
COMMIT_TRAILER_RE = re.compile(r"^Claude-Session: https://claude\.ai/code/session_[A-Za-z0-9]+$")
CO_AUTHOR_RE = re.compile(r"^\s*co-authored-by:", re.IGNORECASE)
FENCE_RE = re.compile(r"^\s*(```|~~~)")
H2_RE = re.compile(r"^## (\S.*)$")
FOOTER_SHAPE = "---, then <Verb> with LLM: <model> | <effort> | Harness: <name>"
PSE_HEADING = "Plain simple English"
PSE_WORD_LIMIT = 55


def normalize(text):
    return [line.rstrip() for line in text.replace("\r\n", "\n").replace("\r", "\n").split("\n")]


def outside_fences(lines):
    inside = False
    for index, line in enumerate(lines):
        if FENCE_RE.match(line):
            inside = not inside
            continue
        if not inside:
            yield index, line


def footer_errors(lines, trailer_re, trailer_name):
    separators = [i for i, line in outside_fences(lines) if line == "---"]
    if not separators:
        return None, [f"no footer: the text has no `---` line; expected {FOOTER_SHAPE}"]
    start = separators[-1]
    rest = lines[start + 1 :]
    if not rest or not FOOTER_LINE_RE.match(rest[0]):
        shown = rest[0] if rest else "(end of text)"
        return start, [f"the line after the last `---` is not a footer line ({FOOTER_SHAPE}): {shown!r}"]
    errors = []
    for line in rest[1:]:
        if line and not FOOTER_LINE_RE.match(line) and not trailer_re.match(line):
            errors.append(
                f"after the `---` footer only footer lines, blank lines and {trailer_name} lines may follow: {line!r}"
            )
    return start, errors


def co_author_errors(lines):
    return [f"Co-authored-by trailer is not allowed: {line.strip()!r}" for line in lines if CO_AUTHOR_RE.match(line)]


def closed_issues(body):
    numbers = []
    for _, line in outside_fences(normalize(body)):
        for match in CLOSING_RE.finditer(INLINE_CODE_RE.sub("", line)):
            if match.group(1) not in numbers:
                numbers.append(match.group(1))
    return numbers


def check_title(title, body):
    if not TITLE_RE.fullmatch(title):
        return [
            f"PR title {title!r} does not match type(scope): summary [C<score>, <model>, <effort>] "
            f"with an optional , plan (or legacy , fableplan) suffix; regex {TITLE_RE.pattern}"
        ]
    closed = closed_issues(body)
    if not closed:
        return []
    scope = TITLE_SCOPE_RE.match(title)
    allowed = [f"#{number}" for number in closed]
    if scope and scope.group(1) in allowed:
        return []
    found = f"({scope.group(1)})" if scope else "no scope"
    return [f"the PR body closes {', '.join(allowed)}, so the title scope must be ({' or '.join(allowed)}); found {found}"]


def check_body(body):
    lines = normalize(body)
    errors = []
    headings = [(i, m.group(1).strip()) for i, line in outside_fences(lines) for m in [H2_RE.match(line)] if m]
    if not headings:
        errors.append("the PR body has no `## ` headings; it needs `## Summary` first and `## Plain simple English` last")
    else:
        if headings[0][1] != "Summary":
            errors.append(f"the first `## ` heading must be `## Summary`, found `## {headings[0][1]}`")
        if headings[-1][1] != PSE_HEADING:
            errors.append(f"the last `## ` heading must be `## {PSE_HEADING}`, found `## {headings[-1][1]}`")
    footer_start, ferrors = footer_errors(lines, PR_TRAILER_RE, "session URL (https://claude.ai/code/session_...)")
    errors.extend(ferrors)
    errors.extend(co_author_errors(lines))
    if headings and headings[-1][1] == PSE_HEADING:
        pse_start = headings[-1][0] + 1
        pse_end = footer_start if footer_start is not None and footer_start >= pse_start else len(lines)
        words = len(" ".join(lines[pse_start:pse_end]).split())
        if words == 0:
            errors.append(f"`## {PSE_HEADING}` is empty")
        elif words >= PSE_WORD_LIMIT:
            errors.append(
                f"`## {PSE_HEADING}` has {words} words before the `---` footer separator; it must be under {PSE_WORD_LIMIT}"
            )
    return errors


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout


def base_exclusions(base, base_ref):
    exclusions = [base]
    if base_ref:
        ref = f"refs/remotes/origin/{base_ref}"
        valid = subprocess.run(["git", "check-ref-format", ref], capture_output=True).returncode == 0
        if valid and subprocess.run(["git", "rev-parse", "--verify", "--quiet", ref], capture_output=True).returncode == 0:
            exclusions.append(ref)
    return exclusions


def check_commits(base, head, base_ref):
    errors = []
    exclusions = base_exclusions(base, base_ref)
    shas = git("rev-list", "--no-merges", "--reverse", head, "--not", *exclusions).split()
    print(f"checking non-merge commits in {head} not in {' '.join(exclusions)}")
    if not shas:
        print("no non-merge commits to check")
    for sha in shas:
        message = git("log", "-1", "--format=%B", sha)
        lines = normalize(message)
        while lines and not lines[-1]:
            lines.pop()
        subject = lines[0] if lines else ""
        _, ferrors = footer_errors(lines, COMMIT_TRAILER_RE, "Claude-Session: trailer")
        problems = ferrors + co_author_errors(lines)
        for problem in problems:
            errors.append(f"commit {sha[:12]} ({subject}): {problem}")
        if not problems:
            print(f"ok: commit {sha[:12]} ({subject})")
    return errors


def annotation(message):
    return message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def require_env(name):
    value = os.environ.get(name)
    if value is None:
        print(f"::error::environment variable {name} is not set")
        sys.exit(2)
    return value


def main(argv):
    if len(argv) != 2 or argv[1] not in ("title", "body", "commits"):
        print("usage: check_pr_metadata.py title|body|commits (reads PR_TITLE and PR_BODY, PR_BODY, or BASE_SHA, HEAD_SHA and optional BASE_REF)")
        return 2
    mode = argv[1]
    if mode == "title":
        errors = check_title(require_env("PR_TITLE"), require_env("PR_BODY"))
    elif mode == "body":
        errors = check_body(require_env("PR_BODY"))
    else:
        errors = check_commits(require_env("BASE_SHA"), require_env("HEAD_SHA"), os.environ.get("BASE_REF", ""))
    for error in errors:
        print(f"::error::{annotation(error)}")
    if errors:
        return 1
    print(f"ok: PR {mode} check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
