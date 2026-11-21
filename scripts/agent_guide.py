#!/usr/bin/env python3
import argparse
import collections
import os
import pathlib
import re
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
GUIDE = "CLAUDE.md"
MIRROR = "AGENTS.md"
RULES_DIR = ".claude/rules"
REGENERATE = "python3 -I scripts/agent_guide.py"
VERIFY = "bash scripts/check_pre_pr.sh agent-docs"
GUIDE_CEILING = 17500
RULE_MAX = 8000
LINE_MAX = 400
MIRROR_MAX = 28000
NAME_RE = re.compile(r"[a-z0-9]+(?:-[a-z0-9]+)*\.md")
MANIFEST_RE = re.compile(r"^- `\.claude/rules/([^`]+)`")
GLOB_RE = re.compile(r'^  - "([^"\\\s]+)"$')
SPAN_RE = re.compile(r"``.+?``|`[^`]+`")
GUARD_RE = re.compile(r"`([\w./*-]+\.(?:go|py|sh))`")
TEST_EXCLUDES = (":(exclude,glob)**/*_test.go", ":(exclude,glob)**/test_*", ":(exclude,glob)**/*.md", ":(exclude,glob).claude/**", ":(exclude)scripts/agent_guide.py", ":(exclude)scripts/check_agent_docs_fixtures.sh")
CLOSURES = (
    ("deploy.md", "names update_git or update_resolve_tool", ("-F", "-e", "update_git", "-e", "update_resolve_tool"), (":(glob)scripts/**",)),
    ("discord.md", "names opsCommandNames or slashCommands(", ("-F", "-e", "opsCommandNames", "-e", "slashCommands("), (":(glob)scheduler/*.go",)),
    ("hedge.md", "names a hedge", ("-F", "-i", "-e", "hedge"), (":(glob)scheduler/*.go",)),
    ("market-feed.md", "names hlInfoPost, feedMu or weight_reserve", ("-F", "-e", "hlInfoPost", "-e", "feedMu", "-e", "weight_reserve"), (":(glob)scheduler/*.go",)),
    ("merge-paper.md", "names LoadConfigReadOnly or resolveChannel(", ("-F", "-e", "LoadConfigReadOnly", "-e", "resolveChannel("), (":(glob)scheduler/*.go",)),
    ("robinhood.md", "names GO_TRADER_ROBINHOOD_SESSION_DIR", ("-F", "-e", "GO_TRADER_ROBINHOOD_SESSION_DIR"), (":(glob)**",)),
    ("ui.md", "names uiPartitionParam or spawnPythonProcessWithEnv", ("-F", "-e", "uiPartitionParam", "-e", "spawnPythonProcessWithEnv"), (":(glob)scheduler/*.go",)),
)


class GuideError(Exception):
    pass


def manifest(text):
    return [m.group(1) for m in map(MANIFEST_RE.match, text.split("\n")) if m]


def parse_rule(name, text):
    lines = text.split("\n")
    if len(lines) < 4 or lines[0] != "---" or lines[1] != "paths:":
        raise GuideError(f"{RULES_DIR}/{name}: frontmatter must start with '---' and 'paths:'")
    globs = []
    i = 2
    while i < len(lines) and lines[i] != "---":
        m = GLOB_RE.match(lines[i])
        if not m:
            raise GuideError(f'{RULES_DIR}/{name}: frontmatter line {i + 1} must be \'  - "<glob>"\'')
        if "/" not in m.group(1):
            raise GuideError(f"{RULES_DIR}/{name}: path {m.group(1)!r} has no slash, so it would match at any depth")
        globs.append(m.group(1))
        i += 1
    if i == len(lines):
        raise GuideError(f"{RULES_DIR}/{name}: frontmatter has no closing '---'")
    if not globs:
        raise GuideError(f"{RULES_DIR}/{name}: paths: lists no glob")
    body = "\n".join(lines[i + 1:])
    if not body.startswith("\n## ") or not body.endswith("\n") or body.endswith("\n\n"):
        raise GuideError(f"{RULES_DIR}/{name}: after the frontmatter put one blank line, a '## ' heading, the bullets, and one final newline")
    return globs, body[1:]


def read_text(root, rel):
    path = root / rel
    if path.is_symlink() or not path.is_file():
        raise GuideError(f"{rel} must be a regular file")
    return path.read_text(encoding="utf-8")


def tracked_rules(root):
    try:
        out = subprocess.run(["git", "-C", str(root), "ls-files", "-z", "--", RULES_DIR], check=True, capture_output=True).stdout
    except (OSError, subprocess.CalledProcessError) as exc:
        raise GuideError(f"git ls-files {RULES_DIR} failed: {exc}")
    return sorted(p[len(RULES_DIR) + 1:] for p in out.decode().split("\0") if p)


def glob_regex(rel, glob):
    out, i = "", 0
    while i < len(glob):
        c = glob[i]
        if glob.startswith("**/", i):
            out += "(?:.*/)?"
            i += 3
        elif glob.startswith("**", i):
            out += ".*"
            i += 2
        elif c == "*":
            out += "[^/]*"
            i += 1
        elif c == "?":
            out += "[^/]"
            i += 1
        elif c in "[]{}!":
            raise GuideError(f"{rel}: path {glob!r} uses {c!r}, which the coverage check cannot match; write plain globs")
        else:
            out += re.escape(c)
            i += 1
    return re.compile(out)


def closure_errors(root, rules):
    errors = []
    by_name = {n: globs for n, globs, _, _ in rules}
    for name, what, pattern, scope in CLOSURES:
        rel = f"{RULES_DIR}/{name}"
        if name not in by_name:
            errors.append(f"{rel}: required rule file is missing; its coverage check cannot run")
            continue
        r = subprocess.run(["git", "-C", str(root), "grep", "-l", "-I", "-z", *pattern, "--", *scope, *TEST_EXCLUDES], capture_output=True)
        if r.returncode not in (0, 1):
            errors.append(f"{rel}: git grep failed: {r.stderr.decode().strip()}")
            continue
        try:
            matchers = [glob_regex(rel, g) for g in by_name[name]]
        except GuideError as exc:
            errors.append(str(exc))
            continue
        for path in sorted(p for p in r.stdout.decode().split("\0") if p):
            if not any(m.fullmatch(path) for m in matchers):
                errors.append(f"{rel}: {path} {what} but matches no paths: glob, so the rule would not load there; add the file to paths: and regenerate")
    return errors


def load(root, check_index=False):
    errors = []
    guide = read_text(root, GUIDE)
    names = manifest(guide)
    if len(set(names)) != len(names):
        errors.append(f"{GUIDE}: the area rule list names a file twice")
    for n in names:
        if not NAME_RE.fullmatch(n):
            errors.append(f"{GUIDE}: area rule name {n!r} must match [a-z0-9-]+.md")
    rules_dir = root / RULES_DIR
    on_disk = []
    if rules_dir.is_symlink():
        errors.append(f"{RULES_DIR} must be a directory, not a symlink")
    elif rules_dir.is_dir():
        for entry in sorted(rules_dir.iterdir()):
            if entry.is_symlink() or not entry.is_file():
                errors.append(f"{RULES_DIR}/{entry.name}: must be a regular file (no symlink, no subdirectory)")
            elif not NAME_RE.fullmatch(entry.name):
                errors.append(f"{RULES_DIR}/{entry.name}: name must match [a-z0-9-]+.md")
            else:
                on_disk.append(entry.name)
    for n in sorted(set(names) - set(on_disk)):
        errors.append(f"{RULES_DIR}/{n}: {GUIDE} lists it, but the file is missing")
    for n in sorted(set(on_disk) - set(names)):
        errors.append(f"{RULES_DIR}/{n}: not listed in the {GUIDE} area rule list")
    if check_index:
        tracked = tracked_rules(root)
        for n in sorted(set(on_disk) - set(tracked)):
            errors.append(f"{RULES_DIR}/{n}: not tracked by git (git add it)")
        for n in sorted(set(tracked) - set(on_disk)):
            errors.append(f"{RULES_DIR}/{n}: tracked by git, but missing from the working tree")
    rules = []
    for n in sorted(set(names) & set(on_disk)):
        text = (rules_dir / n).read_text(encoding="utf-8")
        try:
            globs, body = parse_rule(n, text)
        except GuideError as exc:
            errors.append(str(exc))
            continue
        if len(text.encode()) > RULE_MAX:
            errors.append(f"{RULES_DIR}/{n}: {len(text.encode())} bytes, above the {RULE_MAX}-byte limit")
        rules.append((n, globs, body, text))
    if check_index:
        errors.extend(closure_errors(root, rules))
    for rel, text in [(GUIDE, guide)] + [(f"{RULES_DIR}/{n}", t) for n, _, _, t in rules]:
        for i, line in enumerate(text.split("\n"), 1):
            if len(line.encode()) > LINE_MAX:
                errors.append(f"{rel}:{i}: line is {len(line.encode())} bytes, above the {LINE_MAX}-byte limit; split it into bullets")
    return guide, rules, errors


def compose(guide, rules):
    parts = [
        f"Generated file. Do not edit it. Edit {GUIDE} and the {RULES_DIR} files it lists, then run `{REGENERATE}`.\n",
        "\n",
        guide if guide.endswith("\n") else guide + "\n",
        "\n# Area rules\n",
        "\nClaude Code loads each area below as a path-scoped rule when it reads or edits a file in its scope. Other agents apply each area to the same files.\n",
    ]
    for name, globs, body, _ in rules:
        heading, _, rest = body.partition("\n")
        parts.append(f"\n{heading}\n\nSource {RULES_DIR}/{name}. Scope: {', '.join(globs)}\n\n{rest}")
    parts.append(f"\nEnd of generated guide: {len(rules)} area rule files.\n")
    return "".join(parts)


def rules_text(root=ROOT):
    guide, rules, errors = load(pathlib.Path(root))
    if errors:
        raise GuideError("; ".join(errors))
    return "\n".join([guide] + [body for _, _, body, _ in rules])


def guard_globs(text):
    return sorted({g for g in GUARD_RE.findall(text) if not g.endswith(("/*.go", "/test_*.py"))})


def mirror_errors(root, composed):
    path = root / MIRROR
    if path.is_symlink():
        return [f"{MIRROR} is a symlink; it must be the generated regular file. Run `{REGENERATE}`."]
    if not path.is_file():
        return [f"{MIRROR} is missing. Run `{REGENERATE}`."]
    errors = []
    if path.read_bytes() != composed.encode():
        errors.append(f"{MIRROR} is stale or hand-edited. Run `{REGENERATE}` and commit the result.")
    if len(composed.encode()) > MIRROR_MAX:
        errors.append(f"{MIRROR} would be {len(composed.encode())} bytes, above the {MIRROR_MAX}-byte limit")
    return errors


def write_mirror(root, composed):
    fd, tmp = tempfile.mkstemp(prefix=".AGENTS.md.", dir=root)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(composed.encode())
        os.chmod(tmp, 0o644)
        os.replace(tmp, root / MIRROR)
    except BaseException:
        if os.path.lexists(tmp):
            os.unlink(tmp)
        raise


def git_show(root, rev, path):
    r = subprocess.run(["git", "-C", str(root), "show", f"{rev}:{path}"], capture_output=True)
    if r.returncode != 0:
        raise GuideError(f"git show {rev}:{path} failed: {r.stderr.decode().strip()}")
    return r.stdout.decode()


def rev_sources(root, rev):
    guide = git_show(root, rev, GUIDE)
    r = subprocess.run(["git", "-C", str(root), "ls-tree", "--name-only", f"{rev}:{RULES_DIR}"], capture_output=True)
    names = sorted(n for n in r.stdout.decode().split("\n") if n.endswith(".md")) if r.returncode == 0 else []
    sources = {GUIDE: guide}
    for n in names:
        sources[f"{RULES_DIR}/{n}"] = git_show(root, rev, f"{RULES_DIR}/{n}")
    return sources


def split_atoms(body):
    parts, cur, tick = [], "", False
    for i, c in enumerate(body):
        cur += c
        if c == "`":
            tick = not tick
        elif c == "." and not tick and i + 1 < len(body) and (body[i + 1].isupper() or body[i + 1] == "`"):
            parts.append(cur)
            cur = ""
    if cur:
        parts.append(cur)
    return parts


def atoms(sources):
    out = []
    for rel, text in sources.items():
        tag = "L" if rel == GUIDE else rel.rsplit("/", 1)[-1] + ":L"
        for ln, line in enumerate(text.split("\n"), 1):
            m = re.match(r"^\s*- (.*)$", line)
            if m and not GLOB_RE.match(line):
                for n, atom in enumerate(split_atoms(m.group(1)), 1):
                    out.append((f"{tag}{ln}.{n}", atom))
    return out


def joined(sources):
    return "\n".join(sources.values())


def lost(root, rev):
    old = rev_sources(root, rev)
    guide, rules, errors = load(root)
    if errors:
        raise GuideError("; ".join(errors))
    new = {GUIDE: guide}
    for n, _, _, text in rules:
        new[f"{RULES_DIR}/{n}"] = text
    missing = 0
    print(f"atom ledger: {rev} -> working tree")
    for aid, atom in atoms(old):
        holders = [rel for rel, text in new.items() if atom in text]
        if not holders:
            missing += 1
        print(f"{aid}\t{'+'.join(holders) or 'MISSING'}\t{atom}")
    old_text, new_text = joined(old), joined(new)
    spans = collections.Counter(SPAN_RE.findall(old_text)) - collections.Counter(SPAN_RE.findall(new_text))
    tokens = sorted(set(re.findall(r"\w+", old_text)) - set(re.findall(r"\w+", new_text)))
    og, ng = guard_globs(old_text), guard_globs(new_text)
    print(f"MISSING {missing}")
    print(f"lost code spans {sum(spans.values())}: {' '.join(sorted(spans.elements()))}")
    print(f"lost word tokens {len(tokens)}: {' '.join(tokens)}")
    print(f"protected-path globs: old {len(og)}, new {len(ng)}, removed {sorted(set(og) - set(ng))}, added {sorted(set(ng) - set(og))}")
    return 1 if missing or spans or og != ng else 0


def main():
    ap = argparse.ArgumentParser(description=f"Write {MIRROR} from {GUIDE} and the {RULES_DIR} files it lists.")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--check", action="store_true", help=f"read-only: fail on a bad source or a missing or stale {MIRROR}")
    g.add_argument("--rules-text", action="store_true", help="print the complete rule text with no frontmatter")
    g.add_argument("--lost", metavar="REV", help="print where each rule of REV lives now, and what text is gone")
    args = ap.parse_args()
    root = ROOT
    try:
        if args.lost:
            sys.exit(lost(root, args.lost))
        if args.rules_text:
            sys.stdout.write(rules_text(root))
            return
        guide, rules, errors = load(root, check_index=args.check)
        if errors:
            raise GuideError("\n".join(errors))
        composed = compose(guide, rules)
        if args.check:
            errors = mirror_errors(root, composed)
            if errors:
                raise GuideError("\n".join(errors))
            print(f"agent_guide: {MIRROR} is current ({len(composed.encode())} bytes, {len(rules)} area rule files)")
            return
        if len(composed.encode()) > MIRROR_MAX:
            raise GuideError(f"{MIRROR} would be {len(composed.encode())} bytes, above the {MIRROR_MAX}-byte limit")
        write_mirror(root, composed)
        print(f"agent_guide: wrote {MIRROR} ({len(composed.encode())} bytes, {len(rules)} area rule files)")
    except GuideError as exc:
        for line in str(exc).split("\n"):
            print(f"::error::{line}", file=sys.stderr)
        print(f"Fix the sources, run `{REGENERATE}`, then `{VERIFY}`.", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
