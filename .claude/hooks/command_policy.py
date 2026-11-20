#!/usr/bin/env python3
import collections
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys

GO_TRADER_ROOT_COMMITS = ("444bc81ba17de7824c5a628d0a211f83cc599442",)
GO_TRADER_REMOTE = re.compile(r"github\.com[:/]+richkuo/go-trader(?:\.git)?/*(?=$|[\s/.])", re.IGNORECASE)
PROTECTED_REF = "refs/heads/main"
ACTION_WRAPPER_SHA256 = {
    "git-commit.sh": "82bc286f759fc9f498611c6daaf2c9a36b9aa6e58fd25902802a1f3a9ef15a64",
    "git-env.sh": "4e419a7df3bd7c7e6c378ce60a7fcd19149e0b03db4cb2343035f5408ea9b712",
}
TRUSTED_GIT = ("/usr/bin/git", "/usr/local/bin/git", "/opt/homebrew/bin/git", "/bin/git")
PROBE_TIMEOUT = 5.0
MAX_INPUT_BYTES = 1 << 20
MAX_DEPTH = 12
MAX_STATES = 32
MAX_PROBES = 64
MAX_TOKENS = 50000

UNPARSED_GUARD = re.compile(r"\bgit\b|update\.sh|git-commit\.sh|\$[^\s;&|]+[ \t]+commit\b")
BASH_NOEXEC_FLAGS = re.compile(r"-[enuvx]+\Z")
NAME_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
FD_RE = re.compile(r"\d+(?=[<>])")
META = " \t\r\n;&|()<>"
OPS = ("&>>", "<<<", "<<-", ";;&", "&&", "||", ";;", ";&", "|&", "&>", "<<", ">>", "<>", "<&", ">&", ">|",
       ";", "&", "|", "(", ")", "<", ">")
REDIR_OPS = frozenset(("<", ">", ">>", "<>", "<&", ">&", ">|", "&>", "&>>", "<<", "<<-", "<<<"))
COMPOUND = frozenset(("if", "then", "elif", "else", "fi", "for", "while", "until", "do", "done", "case", "esac",
                      "select", "function", "coproc"))
SHELLS = frozenset(("bash", "sh", "zsh", "dash", "ksh"))
DATA_HEADS = frozenset(("echo", "printf", "cat", "grep", "egrep", "fgrep", "rg", "ls", "head", "tail", "wc", "sort",
                        "uniq", "cut", "tr", "diff", "jq", "tee", "less", "more", "file", "stat", "basename",
                        "dirname", "realpath", "readlink", "date", "sleep", "mkdir", "touch", "cp", "mv", "rm", "ln",
                        "chmod", "which", "type", "printenv", "test", "[", "[[", "true", "false", ":", "pwd",
                        "column", "fold", "nl", "od", "xxd", "shasum", "sha256sum", "md5", "md5sum", "du", "df",
                        "tree", "bat", "shellcheck"))
COMMAND_RUNNERS = frozenset(("xargs", "parallel", "watch", "flock", "doas", "su", "runuser", "chroot", "script",
                             "systemd-run", "unbuffer", "ionice", "chrt", "taskset", "numactl", "strace", "ltrace",
                             "gtimeout", "entr", "awk", "gawk", "mawk", "nawk", "sed", "gsed"))
INPUT_RUNNERS = frozenset(("xargs", "parallel"))
AWK_HEADS = frozenset(("awk", "gawk", "mawk", "nawk"))
SED_HEADS = frozenset(("sed", "gsed"))
WRAPPER_HEADS = frozenset(("command", "builtin", "exec", "nohup", "setsid", "caffeinate", "time", "nice", "timeout",
                           "stdbuf", "env", "sudo"))
FIND_EXEC = frozenset(("-exec", "-execdir", "-ok", "-okdir"))
SHELL_STATE = frozenset(("export", "unset", "declare", "typeset", "local", "readonly", "set", "shopt", "setopt",
                         "unsetopt", "eval", "alias", "unalias", "hash", "enable", "trap", "emulate", "function"))
SET_FLAGS_OK = frozenset(("-e", "+e", "-u", "+u", "-x", "+x", "-v", "+v", "-eu", "-ex", "-eux", "-euo", "-eo", "-uo"))
SET_OPTIONS_OK = frozenset(("pipefail", "errexit", "nounset", "xtrace", "verbose"))
SENSITIVE_ENV = frozenset(("PATH", "HOME", "XDG_CONFIG_HOME", "CDPATH", "BASH_ENV", "ENV", "PWD", "OLDPWD", "IFS",
                           "PROMPT_COMMAND", "SHELLOPTS", "BASHOPTS", "ZDOTDIR"))
SAFE_GIT_ENV = frozenset(("GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS",
                          "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "GIT_OPTIONAL_LOCKS", "GIT_TRACE",
                          "GIT_TRACE2", "GIT_TRACE_PERFORMANCE", "GIT_TRACE_SETUP", "GIT_PROGRESS_DELAY",
                          "GIT_MERGE_AUTOEDIT", "GIT_NO_LAZY_FETCH", "GIT_FLUSH", "GIT_ADVICE"))
GIT_FLAG_OPTS = frozenset(("-p", "--paginate", "-P", "--no-pager", "--no-replace-objects", "--no-lazy-fetch",
                           "--no-optional-locks", "--no-advice", "--literal-pathspecs", "--glob-pathspecs",
                           "--noglob-pathspecs", "--icase-pathspecs", "--exec-path"))
GIT_INFO_OPTS = frozenset(("-v", "--version", "-h", "--help", "--html-path", "--man-path", "--info-path"))
GIT_DENY_CONFIG = ("core.worktree", "core.bare", "extensions.worktreeconfig", "include.", "includeif.")
GIT_ALWAYS_TAINT = frozenset(("update-ref", "bisect", "init", "clone", "rebase", "submodule", "filter-branch",
                              "filter-repo", "gui", "citool"))
GIT_SAFE = frozenset((
    "add", "am", "annotate", "apply", "archive", "backfill", "blame", "bundle", "cat-file", "check-attr",
    "check-ignore", "check-mailmap", "check-ref-format", "cherry", "cherry-pick", "clean", "column", "commit-graph",
    "commit-tree", "count-objects", "credential", "describe", "diff", "diff-files", "diff-index", "diff-tree",
    "difftool", "fetch", "fetch-pack", "fmt-merge-msg", "for-each-ref", "for-each-repo", "format-patch", "fsck", "gc",
    "get-tar-commit-id", "grep", "hash-object", "help", "hook", "index-pack", "interpret-trailers", "last-modified",
    "log", "ls-files", "ls-remote", "ls-tree", "mailinfo", "mailsplit", "maintenance", "merge", "merge-base",
    "merge-file", "merge-tree", "mergetool", "mktag", "mktree", "multi-pack-index", "mv", "name-rev", "notes",
    "pack-objects", "pack-refs", "patch-id", "prune", "prune-packed", "pull", "push", "range-diff", "read-tree",
    "reflog", "refs", "remote", "repack", "replace", "replay", "repo", "request-pull", "rerere", "reset", "restore",
    "rev-list", "rev-parse", "revert", "rm", "send-email", "shortlog", "show", "show-branch", "show-ref",
    "sparse-checkout", "stage", "stash", "status", "stripspace", "tag", "unpack-objects", "update-index", "var",
    "verify-commit", "verify-pack", "verify-tag", "version", "whatchanged", "write-tree", "lfs",
))
GIT_NON_COMMIT = GIT_SAFE | GIT_ALWAYS_TAINT | {"checkout", "switch", "branch", "config", "symbolic-ref", "worktree"}
GIT_BRANCH_CHANGE = GIT_NON_COMMIT - GIT_SAFE
SSH_OPERAND = frozenset("BbcDEeFIiJLlmOoPpQRSWw")

State = collections.namedtuple("State", "cwd taint over")


def literal_word(text):
    w = Word()
    w.text = [text]
    w.raw = text
    return w


XARGS_FLAGS = frozenset("0rtxpo")
XARGS_ARG_FLAGS = frozenset("nLsPdaE")
XARGS_LONG = frozenset(("--null", "--no-run-if-empty", "--verbose", "--exit", "--interactive", "--open-tty",
                        "--show-limits", "--max-args", "--max-lines", "--max-procs", "--max-chars", "--delimiter",
                        "--arg-file", "--process-slot-var"))
XARGS_LONG_ARG = frozenset(("--max-args", "--max-procs", "--max-chars", "--delimiter", "--arg-file",
                            "--process-slot-var"))


def replaces_words(runner_args):
    i = 0
    while i < len(runner_args):
        v = runner_args[i].value
        if v is None or not v.startswith("-") or v == "-":
            return True
        if v == "--":
            return i + 1 != len(runner_args)
        if v.startswith("--"):
            name = v.split("=", 1)[0]
            if name not in XARGS_LONG:
                return True
            i += 2 if name in XARGS_LONG_ARG and "=" not in v else 1
            continue
        j = 1
        consumed = False
        while j < len(v):
            if v[j] in XARGS_FLAGS:
                j += 1
                continue
            if v[j] in XARGS_ARG_FLAGS:
                consumed = j + 1 == len(v)
                break
            if v[j] == "l":
                break
            return True
        i += 2 if consumed else 1
    return False


def skip_delimited(script, i, d):
    while i < len(script) and script[i] != d:
        i += 2 if script[i] == "\\" else 1
    return i + 1


def sed_runs_commands(script):
    i, n = 0, len(script)
    while i < n:
        c = script[i]
        if c in " \t\n;{}!,$~+" or c.isdigit():
            i += 1
        elif c == "#":
            e = script.find("\n", i)
            i = n if e < 0 else e + 1
        elif c in "/\\":
            if c == "\\":
                if i + 1 >= n:
                    return True
                i = skip_delimited(script, i + 2, script[i + 1])
            else:
                i = skip_delimited(script, i + 1, "/")
            while i < n and script[i] in "IM":
                i += 1
        elif c in "sy":
            if i + 1 >= n:
                return True
            d = script[i + 1]
            i = skip_delimited(script, skip_delimited(script, i + 2, d), d)
            while c == "s" and i < n and script[i] not in ";\n}":
                if script[i] == "e":
                    return True
                if script[i] == "w":
                    break
                i += 1
            if c == "s" and i < n and script[i] == "w":
                e = script.find("\n", i)
                i = n if e < 0 else e + 1
        elif c in "btT:v":
            while i < n and script[i] not in ";\n":
                i += 1
        elif c in "aicrwRW":
            e = script.find("\n", i)
            i = n if e < 0 else e + 1
        elif c in "pPdDgGhHnNxz=FlLqQ":
            i += 1
        else:
            return True
    return False


SED_FLAGS = frozenset("nErsuz")
SED_LONG = frozenset(("--quiet", "--silent", "--regexp-extended", "--separate", "--unbuffered", "--null-data",
                      "--posix", "--debug", "--sandbox", "--follow-symlinks", "--in-place", "--line-length",
                      "--expression"))
AWK_LONG_ARG = frozenset(("--field-separator", "--assign"))
AWK_EXEC = ("system", "|", "@", "getline")


def sed_program_runs(vals):
    scripts = []
    first = None
    seen_e = False
    operands_only = False
    i = 0
    while i < len(vals):
        v = vals[i]
        i += 1
        if operands_only or not v.startswith("-") or v == "-":
            if first is None and not seen_e:
                first = v
            continue
        if v == "--":
            operands_only = True
            continue
        if v.startswith("--"):
            name, eq, val = v.partition("=")
            if name not in SED_LONG:
                return True
            if name == "--expression":
                if not eq:
                    if i >= len(vals):
                        return True
                    val = vals[i]
                    i += 1
                scripts.append(val)
                seen_e = True
            elif name == "--line-length" and not eq:
                i += 1
            continue
        j = 1
        while j < len(v):
            ch = v[j]
            if ch in SED_FLAGS:
                j += 1
            elif ch == "i":
                if j + 1 == len(v) and i < len(vals) and (vals[i] == "" or
                                                          (vals[i].startswith(".") and " " not in vals[i])):
                    i += 1
                break
            elif ch in "el":
                val = v[j + 1:]
                if not val:
                    if i >= len(vals):
                        return True
                    val = vals[i]
                    i += 1
                if ch == "e":
                    scripts.append(val)
                    seen_e = True
                break
            else:
                return True
    if first is not None:
        scripts.append(first)
    return not scripts or any(sed_runs_commands(x) for x in scripts)


def awk_program_runs(vals):
    i = 0
    while i < len(vals):
        v = vals[i]
        if v == "--":
            i += 1
            break
        if not v.startswith("-") or v == "-":
            break
        if v.startswith("--"):
            name, eq, _ = v.partition("=")
            if name not in AWK_LONG_ARG:
                return True
            i += 1 if eq else 2
            continue
        if v[1] not in "Fv":
            return True
        i += 1 if len(v) > 2 else 2
    if i >= len(vals):
        return True
    return any(x in vals[i] for x in AWK_EXEC)


def text_program_runs(base, args):
    vals = [a.value for a in args]
    if any(v is None for v in vals):
        return True
    if base in SED_HEADS:
        return sed_program_runs(vals)
    return awk_program_runs(vals)


def gh_switches(vals):
    return vals[:2] == ["pr", "checkout"] or vals[:1] == ["co"] or vals[:2] in (["repo", "clone"], ["repo", "fork"])


class ParseError(Exception):
    pass


class Ctx(object):
    def __init__(self):
        self.complex = []
        self.opaque = []
        self.commands = []


class Word(object):
    __slots__ = ("text", "unq", "dynamic", "quoted", "subs", "assign", "raw")

    def __init__(self):
        self.text = []
        self.unq = []
        self.dynamic = False
        self.quoted = False
        self.subs = []
        self.assign = None
        self.raw = ""

    @property
    def value(self):
        if self.dynamic:
            return None
        return "".join(self.text)

    @property
    def assign_value(self):
        if self.assign is None or self.dynamic:
            return None
        return "".join(self.text)[self.assign[1]:]


def decode_ansi_c(body):
    out = []
    i = 0
    simple = {"a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b", "f": "\f", "n": "\n", "r": "\r", "t": "\t",
              "v": "\v", "\\": "\\", "'": "'", '"': '"', "?": "?"}
    while i < len(body):
        c = body[i]
        if c != "\\" or i + 1 >= len(body):
            out.append(c)
            i += 1
            continue
        n = body[i + 1]
        if n in simple:
            out.append(simple[n])
            i += 2
            continue
        m = re.match(r"x([0-9A-Fa-f]{1,2})|u([0-9A-Fa-f]{1,4})|U([0-9A-Fa-f]{1,8})|([0-7]{1,3})", body[i + 1:])
        if m:
            digits = next(g for g in m.groups() if g is not None)
            base = 8 if m.group(4) is not None else 16
            try:
                out.append(chr(int(digits, base)))
            except (ValueError, OverflowError):
                out.append("?")
            i += 1 + m.end()
            continue
        if n == "c" and i + 2 < len(body):
            out.append(chr(ord(body[i + 2]) & 31))
            i += 3
            continue
        out.append("\\" + n)
        i += 2
    return "".join(out)


class Parser(object):
    def __init__(self, src, depth, ctx):
        if depth > MAX_DEPTH:
            raise ParseError("nesting is too deep")
        self.src = src
        self.n = len(src)
        self.pos = 0
        self.depth = depth
        self.ctx = ctx
        self.pending = []
        self.peeked = None
        self.tokens = 0

    def parse(self):
        lst = self.parse_list(None)
        if self.peek()[0] != "eof":
            raise ParseError("unexpected text after the command")
        return lst

    def child(self, text):
        return Parser(text, self.depth + 1, self.ctx)

    def peek(self):
        if self.peeked is None:
            self.peeked = self.lex()
        return self.peeked

    def take(self):
        t = self.peek()
        self.peeked = None
        return t

    def lex(self):
        self.tokens += 1
        if self.tokens > MAX_TOKENS:
            raise ParseError("the command is too long")
        s = self.src
        while self.pos < self.n:
            c = s[self.pos]
            if c in " \t\r":
                self.pos += 1
            elif s.startswith("\\\n", self.pos):
                self.pos += 2
            elif c == "#":
                while self.pos < self.n and s[self.pos] != "\n":
                    self.pos += 1
            else:
                break
        if self.pos >= self.n:
            if self.pending:
                self.read_heredocs()
            return ("eof",)
        c = s[self.pos]
        if c == "\n":
            self.pos += 1
            self.read_heredocs()
            return ("nl",)
        fd = None
        m = FD_RE.match(s, self.pos)
        if m and not s.startswith("<(", m.end()) and not s.startswith(">(", m.end()):
            fd = m.group(0)
            self.pos = m.end()
        elif (s.startswith("<(", self.pos) or s.startswith(">(", self.pos)):
            return ("word", self.read_word())
        for op in OPS:
            if s.startswith(op, self.pos):
                if fd is not None and op not in REDIR_OPS:
                    raise ParseError("a file descriptor number without a redirection")
                self.pos += len(op)
                return ("op", op, fd)
        if fd is not None:
            raise ParseError("a file descriptor number without a redirection")
        return ("word", self.read_word())

    def read_word(self):
        s = self.src
        w = Word()
        start = self.pos
        plain = True
        while self.pos < self.n:
            c = s[self.pos]
            if c in "<>" and s.startswith("(", self.pos + 1) and self.pos == start:
                self.pos += 2
                w.subs.append((self.parse_list(")"), True))
                w.dynamic = True
                plain = False
                continue
            if c in META:
                break
            if c == "\\":
                if self.pos + 1 >= self.n:
                    w.text.append("\\")
                    self.pos += 1
                    continue
                nxt = s[self.pos + 1]
                self.pos += 2
                if nxt == "\n":
                    continue
                w.text.append(nxt)
                w.quoted = True
                plain = False
                continue
            if c == "'":
                end = s.find("'", self.pos + 1)
                if end < 0:
                    raise ParseError("an unterminated single quote")
                w.text.append(s[self.pos + 1:end])
                self.pos = end + 1
                w.quoted = True
                plain = False
                continue
            if c == '"':
                self.pos += 1
                self.read_dq(w, True)
                w.quoted = True
                plain = False
                continue
            if c == "$":
                plain = False
                self.read_dollar(w, False)
                continue
            if c == "`":
                plain = False
                self.read_backtick(w)
                continue
            if c == "~" and self.pos == start:
                m = re.compile(r"~([^/\s;&|()<>'\"`$\\]*)").match(s, self.pos)
                if m.group(1) == "" and os.environ.get("HOME"):
                    w.text.append(os.environ["HOME"])
                else:
                    w.text.append(m.group(0))
                    w.dynamic = True
                self.pos = m.end()
                plain = False
                continue
            if c == "=" and plain and w.assign is None:
                name = "".join(w.text)
                bare = name[:-1] if name.endswith("+") else name
                if NAME_RE.match(bare):
                    w.assign = (bare, len(name) + 1)
            w.text.append(c)
            w.unq.append(c)
            self.pos += 1
        w.raw = s[start:self.pos]
        if self.pos == start:
            raise ParseError("an empty word")
        unq = "".join(w.unq)
        if "*" in unq or "?" in unq or ("[" in unq and "]" in unq[unq.index("["):]):
            w.dynamic = True
        if "{" in unq and "}" in unq[unq.index("{"):] and ("," in unq or ".." in unq):
            w.dynamic = True
        return w

    def read_dq(self, w, terminated):
        s = self.src
        while True:
            if self.pos >= self.n:
                if terminated:
                    raise ParseError("an unterminated double quote")
                return
            c = s[self.pos]
            if c == '"' and terminated:
                self.pos += 1
                return
            if c == "\\":
                nxt = s[self.pos + 1] if self.pos + 1 < self.n else ""
                if nxt in ("$", "`", '"', "\\"):
                    w.text.append(nxt)
                    self.pos += 2
                    continue
                if nxt == "\n":
                    self.pos += 2
                    continue
                w.text.append("\\")
                self.pos += 1
                continue
            if c == "$":
                self.read_dollar(w, True)
                continue
            if c == "`":
                self.read_backtick(w)
                continue
            w.text.append(c)
            self.pos += 1

    def match_close(self, i, opener, closer, depth):
        s = self.src
        while i < self.n:
            c = s[i]
            if c == "\\":
                i += 2
                continue
            if c == "'" and opener == "{":
                e = s.find("'", i + 1)
                if e < 0:
                    break
                i = e + 1
                continue
            if c == opener:
                depth += 1
            elif c == closer:
                depth -= 1
                if depth == 0:
                    return i + 1
            i += 1
        raise ParseError("an unterminated expansion")

    def read_dollar(self, w, in_dq):
        s = self.src
        p = self.pos
        nxt = s[p + 1] if p + 1 < self.n else ""
        if nxt == "'" and not in_dq:
            i = p + 2
            while i < self.n and s[i] != "'":
                i += 2 if s[i] == "\\" else 1
            if i >= self.n:
                raise ParseError("an unterminated $'...' quote")
            w.text.append(decode_ansi_c(s[p + 2:i]))
            w.quoted = True
            self.pos = i + 1
            return
        if nxt == '"' and not in_dq:
            self.pos = p + 2
            self.read_dq(w, True)
            w.quoted = True
            return
        if s.startswith("$((", p):
            end = self.match_close(p + 3, "(", ")", 2)
            self.ctx.opaque.append(s[p + 3:end - 2])
            w.text.append(s[p:end])
            w.dynamic = True
            self.pos = end
            return
        if nxt == "(":
            self.pos = p + 2
            w.subs.append((self.parse_list(")"), False))
            w.dynamic = True
            return
        if nxt == "{":
            end = self.match_close(p + 2, "{", "}", 1)
            self.ctx.opaque.append(s[p + 2:end - 1])
            w.text.append(s[p:end])
            w.dynamic = True
            self.pos = end
            return
        m = re.compile(r"[A-Za-z_][A-Za-z0-9_]*|[0-9@*#?$!-]").match(s, p + 1)
        if m:
            w.text.append(s[p:m.end()])
            w.dynamic = True
            self.pos = m.end()
            return
        w.text.append("$")
        w.unq.append("$")
        self.pos = p + 1

    def read_backtick(self, w):
        s = self.src
        i = self.pos + 1
        buf = []
        while True:
            if i >= self.n:
                raise ParseError("an unterminated backquote")
            c = s[i]
            if c == "\\" and i + 1 < self.n and s[i + 1] in "$`\\":
                buf.append(s[i + 1])
                i += 2
                continue
            if c == "`":
                break
            buf.append(c)
            i += 1
        self.pos = i + 1
        w.subs.append((self.child("".join(buf)).parse(), False))
        w.dynamic = True

    def read_heredocs(self):
        s = self.src
        pending, self.pending = self.pending, []
        for h in pending:
            lines = []
            while self.pos < self.n:
                e = s.find("\n", self.pos)
                if e < 0:
                    line = s[self.pos:]
                    self.pos = self.n
                else:
                    line = s[self.pos:e]
                    self.pos = e + 1
                if h["strip"]:
                    line = line.lstrip("\t")
                if line == h["delim"]:
                    break
                lines.append(line)
            body = "\n".join(lines)
            h["redir"]["body"] = body
            h["redir"]["quoted"] = h["quoted"]
            if not h["quoted"]:
                bw = Word()
                self.child(body).read_dq(bw, False)
                h["redir"]["subs"].extend(bw.subs)

    def is_word(self, t, value):
        return t[0] == "word" and not t[1].quoted and not t[1].dynamic and "".join(t[1].text) == value

    def parse_list(self, end):
        items = []
        while True:
            t = self.peek()
            if t[0] == "nl":
                self.take()
                continue
            if t[0] == "eof":
                if end is not None:
                    raise ParseError("a missing " + end)
                break
            if t[0] == "op" and t[1] == ")":
                if end == ")":
                    self.take()
                    break
                raise ParseError("an unexpected )")
            if end == "}" and self.is_word(t, "}"):
                self.take()
                break
            if t[0] == "op" and t[1] in (";;", ";&", ";;&"):
                self.ctx.complex.append("case")
                self.take()
                continue
            ao = self.parse_andor()
            t = self.peek()
            sep = None
            if t[0] == "op" and t[1] in (";", "&"):
                sep = t[1]
                self.take()
            elif t[0] == "nl":
                sep = "\n"
                self.take()
            items.append((ao, sep))
            if sep is None:
                t = self.peek()
                if t[0] == "eof" or (t[0] == "op" and t[1] in (")", ";;", ";&", ";;&")):
                    continue
                if end == "}" and self.is_word(t, "}"):
                    continue
                raise ParseError("an unexpected token")
        return ("list", items)

    def parse_andor(self):
        first = self.parse_pipeline()
        rest = []
        while True:
            t = self.peek()
            if t[0] == "op" and t[1] in ("&&", "||"):
                self.take()
                while self.peek()[0] == "nl":
                    self.take()
                rest.append((t[1], self.parse_pipeline()))
            else:
                break
        return ("andor", first, rest)

    def parse_pipeline(self):
        neg = False
        while True:
            t = self.peek()
            if self.is_word(t, "!"):
                self.take()
                neg = not neg
                continue
            if self.is_word(t, "time"):
                self.take()
                if self.is_word(self.peek(), "-p"):
                    self.take()
                continue
            break
        cmds = [self.parse_command()]
        while True:
            t = self.peek()
            if t[0] == "op" and t[1] in ("|", "|&"):
                self.take()
                while self.peek()[0] == "nl":
                    self.take()
                cmds.append(self.parse_command())
            else:
                break
        return ("pipe", cmds, neg)

    def parse_command(self):
        t = self.peek()
        if t[0] == "op" and t[1] == "(":
            self.take()
            inner = self.parse_list(")")
            return ("sub", inner, self.parse_redirs())
        if self.is_word(t, "{"):
            self.take()
            inner = self.parse_list("}")
            return ("group", inner, self.parse_redirs())
        if t[0] == "word" and not t[1].quoted and not t[1].dynamic and "".join(t[1].text) in COMPOUND:
            self.ctx.complex.append("".join(t[1].text))
        return self.parse_simple()

    def parse_redirs(self):
        redirs = []
        while True:
            t = self.peek()
            if t[0] == "op" and t[1] in REDIR_OPS:
                self.take()
                redirs.append(self.read_redir(t))
            else:
                return redirs

    def read_redir(self, t):
        nt = self.take()
        if nt[0] != "word":
            raise ParseError("a redirection without a target")
        r = {"op": t[1], "fd": t[2], "word": nt[1], "body": None, "quoted": False, "subs": []}
        if t[1] in ("<<", "<<-"):
            if nt[1].dynamic:
                raise ParseError("a here-document delimiter that is not literal")
            self.pending.append({"delim": "".join(nt[1].text), "strip": t[1] == "<<-", "quoted": nt[1].quoted,
                                 "redir": r})
        return r

    def parse_simple(self):
        assigns, words, redirs = [], [], []
        while True:
            t = self.peek()
            if t[0] == "word":
                self.take()
                if not words and t[1].assign is not None:
                    assigns.append(t[1])
                else:
                    words.append(t[1])
                continue
            if t[0] == "op" and t[1] in REDIR_OPS:
                self.take()
                redirs.append(self.read_redir(t))
                continue
            break
        if not assigns and not words and not redirs:
            raise ParseError("an empty command")
        self.ctx.commands.append(words)
        return ("simple", assigns, words, redirs)


class Inv(object):
    def __init__(self):
        self.envops = []
        self.chdirs = []
        self.cwd_unknown = None
        self.external = False
        self.background = False
        self.problems = []


class Target(object):
    def __init__(self, gitdir, common, top, shallow):
        self.gitdir = gitdir
        self.common = common
        self.top = top
        self.shallow = shallow


def trusted_git():
    for path in TRUSTED_GIT:
        if os.path.isfile(path) and os.access(path, os.X_OK):
            return path
    return None


def sensitive_env(name):
    if name in SENSITIVE_ENV:
        return True
    if name.startswith("GIT_AUTHOR_") or name.startswith("GIT_COMMITTER_"):
        return False
    return name.startswith("GIT_") and name not in SAFE_GIT_ENV


def simulate_update_args(values):
    restart = False
    update_all = False
    clear = False
    rsync = None
    i = 0
    while i < len(values):
        a = values[i]
        if a is None:
            return {"kind": "unknown", "restart": restart}
        if a == "--restart":
            restart = True
            i += 1
        elif a in ("--restart-mode", "--update-all-root", "--rsync-from", "--unit", "--service"):
            if i + 1 >= len(values):
                return {"kind": "error", "restart": restart}
            nv = values[i + 1]
            if nv is None:
                return {"kind": "unknown", "restart": restart}
            if a in ("--unit", "--service") and (nv.strip() == "" or nv.strip().startswith("--")):
                return {"kind": "error", "restart": restart}
            if a == "--rsync-from":
                rsync = nv.strip()
            i += 2
        elif a.startswith("--restart-mode=") or a.startswith("--update-all-root="):
            i += 1
        elif a.startswith("--rsync-from="):
            rsync = a.split("=", 1)[1].strip()
            i += 1
        elif a.startswith("--unit=") or a.startswith("--service="):
            uv = a.split("=", 1)[1].strip()
            if uv == "" or uv.startswith("--"):
                return {"kind": "error", "restart": restart}
            i += 1
        elif a == "--all":
            update_all = True
            i += 1
        elif a == "--clear-recovery":
            clear = True
            i += 1
        elif a in ("-h", "--help"):
            return {"kind": "help", "restart": restart}
        else:
            return {"kind": "error", "restart": restart}
    return {"kind": "parsed", "restart": restart, "all": update_all, "clear": clear, "rsync": rsync}


class Analyzer(object):
    def __init__(self, ctx, text):
        self.ctx = ctx
        self.text = text
        self.denials = []
        self.fs_taint = []
        self.branch_changes = []
        self.unchecked_branch = []
        self.commit_seen = False
        self.alias_stack = []
        self.concurrent_commit = False
        self.remote = 0
        self.env_stack = []
        self.cache = {}
        self.probes = 0
        self.git = trusted_git()

    def deny(self, msg):
        if msg not in self.denials:
            self.denials.append(msg)

    def cap(self, states):
        if len(states) > MAX_STATES:
            return {State(None, "too many possible paths", frozenset())}
        return states

    def eval_list(self, node, states, conc):
        items = node[1]
        cur = set(states)
        last = (set(states), set())
        for ao, sep in items:
            if not cur:
                break
            if sep == "&":
                self.eval_andor(ao, cur, True)
                last = (set(cur), set())
            else:
                s, f = self.eval_andor(ao, cur, conc)
                cur = self.cap(s | f)
                last = (s, f)
        return last

    def eval_andor(self, ao, states, conc):
        s, f = self.eval_pipe(ao[1], states, conc)
        for op, pl in ao[2]:
            if op == "&&":
                if s:
                    s2, f2 = self.eval_pipe(pl, s, conc)
                    s, f = s2, f | f2
            elif f:
                s2, f2 = self.eval_pipe(pl, f, conc)
                s, f = s | s2, f2
        return s, f

    def eval_pipe(self, pl, states, conc):
        cmds = pl[1]
        if len(cmds) == 1:
            s, f = self.eval_cmd(cmds[0], states, conc)
        else:
            for c in cmds[:-1]:
                self.eval_child(lambda st, c=c: self.eval_cmd(c, {st}, True), states)
            s, f = self.eval_cmd(cmds[-1], states, True)
            u = set(states) | s | f
            s, f = u, set(u)
        if pl[2]:
            s, f = f, s
        return s, f

    def eval_child(self, fn, states):
        succ, fail = set(), set()
        for st in states:
            s, f = fn(st)
            succ |= {st._replace(over=r.over) for r in s}
            fail |= {st._replace(over=r.over) for r in f}
        return succ, fail

    def eval_subs(self, words, redirs, states, conc):
        nodes = []
        for w in words:
            nodes.extend(w.subs)
        for r in redirs:
            nodes.extend(r["word"].subs + r["subs"])
        for node, c2 in nodes:
            s, f = self.eval_child(lambda st, node=node, c2=c2: self.eval_list(node, {st}, conc or c2), states)
            states = self.cap(s | f) or states
        return states

    def eval_cmd(self, cmd, states, conc):
        kind = cmd[0]
        if kind == "sub":
            states = self.eval_subs([], cmd[2], states, conc)
            return self.eval_child(lambda st: self.eval_list(cmd[1], {st}, conc), states)
        if kind == "group":
            states = self.eval_subs([], cmd[2], states, conc)
            return self.eval_list(cmd[1], states, conc)
        _, assigns, words, redirs = cmd
        states = self.eval_subs(assigns + words, redirs, states, conc)
        words = list(words)
        if self.ctx.complex:
            while words and not words[0].quoted and words[0].value in COMPOUND:
                words.pop(0)
        if not words:
            taint = None
            for a in assigns:
                if sensitive_env(a.assign[0]):
                    taint = "an assignment to " + a.assign[0]
            if taint:
                states = {st._replace(taint=st.taint or taint) for st in states}
            return set(states), set(states)
        inv = Inv()
        for a in assigns:
            inv.envops.append(("set", a.assign[0], a.assign_value))
        return self.dispatch(words, inv, redirs, states, conc)

    def word_hazards(self, words, depth=0):
        found = set()
        for word in words:
            if word.quoted or word.value not in COMPOUND:
                if word.value is not None and os.path.basename(word.value) in DATA_HEADS:
                    return found
                break
        for i, word in enumerate(words):
            value = word.value
            if value is None:
                if i + 1 < len(words) and words[i + 1].value == "commit":
                    found.add("guarded")
                value = "".join(word.text)
            base = os.path.basename(value)
            if base in ("update.sh", "git-commit.sh"):
                found.add("guarded")
            elif base == "git":
                args = words[i + 1:]
                _, _, _, sub, status = self.git_global(args)
                if status == "dynamic" or (status == "ok" and args[sub].value not in GIT_NON_COMMIT):
                    found.add("guarded")
                elif self.input_fed_git(words, i, status):
                    found |= {"guarded", "branch"}
                elif status == "ok" and args[sub].value in GIT_BRANCH_CHANGE:
                    found.add("branch")
            elif base == "gh" and gh_switches([a.value for a in words[i + 1:i + 3]]):
                found.add("branch")
            if word.quoted and any(c.isspace() for c in value):
                found |= self.text_hazards(value, depth + 1)
        return found

    def input_fed_git(self, words, i, status):
        for j in range(i):
            v = words[j].value
            if v is None or os.path.basename(v) not in INPUT_RUNNERS:
                continue
            if status in ("none", "info") or os.path.basename(v) == "parallel" \
                    or replaces_words(words[j + 1:i]):
                return True
        return False

    def text_hazards(self, text, depth=0):
        ctx = Ctx()
        try:
            Parser(text, depth, ctx).parse()
        except ParseError:
            return {"guarded", "branch"} if UNPARSED_GUARD.search(text) else set()
        found = set()
        for words in ctx.commands:
            found |= self.word_hazards(words, depth)
        for fragment in ctx.opaque:
            found |= self.text_hazards(fragment, depth + 1)
        return found

    def guarded_words(self, words, depth=0):
        return "guarded" in self.word_hazards(words, depth)

    def guarded_text(self, text, depth=0):
        return "guarded" in self.text_hazards(text, depth)

    def note_unchecked(self, hazards, why):
        if "branch" in hazards:
            self.add_taint("a branch change inside %s" % why)
            self.unchecked_branch.append(why)

    def dynamic_guard(self):
        return bool(UNPARSED_GUARD.search(self.text))

    def unsupported(self, words, states, why, dynamic=False):
        hazards = self.word_hazards(words)
        if "guarded" in hazards:
            self.deny("a guarded call appears in a form the policy cannot check (%s)" % why)
        elif dynamic and self.dynamic_guard():
            self.deny("this command runs a command or shell text that is not literal (%s), and the call mentions git, "
                      "the update script or the action commit wrapper. Run the commit or the update as its own "
                      "literal command." % why)
        self.note_unchecked(hazards, why)
        return set(states), set(states)

    def dispatch(self, words, inv, redirs, states, conc):
        while True:
            if not words:
                return set(states), set(states)
            head = words[0]
            hv = head.value
            if hv is None:
                return self.unsupported(words, states, "the command name is not literal", True)
            base = os.path.basename(hv)
            rest = self.strip_wrapper(base, hv, words, inv)
            if rest is None:
                break
            if rest == "data":
                return set(states), set(states)
            if rest == "unsupported":
                return self.unsupported(words, states, "the options of " + base + " are not supported",
                                        any(w.value is None for w in words[1:]))
            if inv.background:
                conc = True
            words = rest
        args = words[1:]
        if base == "git":
            return self.do_git(args, inv, states, conc)
        if base == "git-commit.sh":
            return self.do_action_wrapper(head, inv, states, conc)
        if base == "update.sh":
            return self.do_update(head, args, inv, states, False)
        if base in SHELLS:
            return self.do_shell(args, inv, redirs, states, conc, words)
        if base == "ssh":
            return self.do_ssh(args, inv, states, conc, words)
        if not inv.external and hv in ("source", "."):
            return self.do_source(args, inv, states, conc, words)
        if not inv.external and hv in ("cd", "pushd", "popd", "chdir"):
            return self.do_cd(hv, args, states)
        if not inv.external and hv in SHELL_STATE:
            return self.do_shell_state(hv, args, states, words)
        if base == "gh":
            self.do_gh(args)
            return set(states), set(states)
        if hv == "exec" and not args:
            return set(states), set(states)
        if base in DATA_HEADS:
            return set(states), set(states)
        find_exec = base == "find" and any(a.value is None or a.value in FIND_EXEC for a in args)
        runner = base in COMMAND_RUNNERS or find_exec
        inner = (base in INPUT_RUNNERS or find_exec) and any(
            a.value is not None and os.path.basename(a.value) in SHELLS | WRAPPER_HEADS | COMMAND_RUNNERS | {"ssh"}
            for a in args)
        if base in AWK_HEADS | SED_HEADS:
            inner = text_program_runs(base, args)
        return self.unsupported(words, states, "the command " + base + " is not a supported wrapper",
                                inner or (runner and any(a.value is None for a in args)))

    def strip_wrapper(self, base, hv, words, inv):
        args = words[1:]
        vals = [a.value for a in args]
        if hv in ("command", "builtin"):
            i = 0
            while i < len(vals) and vals[i] is not None and vals[i].startswith("-"):
                if vals[i] in ("-v", "-V"):
                    return "data"
                if vals[i] != "-p":
                    return "unsupported"
                i += 1
            if i < len(vals) and vals[i] is None:
                return "unsupported"
            return args[i:]
        if hv == "exec":
            i = 0
            while i < len(vals):
                if vals[i] is None:
                    return "unsupported"
                if vals[i] in ("-c", "-l"):
                    i += 1
                elif vals[i] == "-a":
                    i += 2
                elif vals[i] == "--":
                    i += 1
                    break
                else:
                    break
            if i >= len(vals):
                return None
            return args[i:]
        if base in ("nohup", "setsid", "caffeinate") and base == hv.split("/")[-1]:
            i = 0
            while i < len(vals) and vals[i] is not None and vals[i].startswith("-"):
                i += 1
            if i < len(vals) and vals[i] is None:
                return "unsupported"
            inv.external = True
            return args[i:]
        if base == "time":
            i = 0
            while i < len(vals) and vals[i] is not None and vals[i].startswith("-"):
                if vals[i] in ("-o", "-f", "--output", "--format"):
                    i += 1
                i += 1
            if i < len(vals) and vals[i] is None:
                return "unsupported"
            inv.external = True
            return args[i:]
        if base == "nice":
            i = 0
            while i < len(vals):
                v = vals[i]
                if v is None:
                    return "unsupported"
                if v == "-n":
                    i += 2
                elif v.startswith("-") and v != "--":
                    i += 1
                elif v == "--":
                    i += 1
                    break
                else:
                    break
            inv.external = True
            return args[i:]
        if base == "timeout":
            i = 0
            while i < len(vals):
                v = vals[i]
                if v is None:
                    return "unsupported"
                if v in ("-s", "-k", "--signal", "--kill-after"):
                    i += 2
                elif v.startswith("-") and v != "--":
                    i += 1
                elif v == "--":
                    i += 1
                    break
                else:
                    break
            inv.external = True
            return args[i + 1:]
        if base == "stdbuf":
            i = 0
            while i < len(vals):
                v = vals[i]
                if v is None:
                    return "unsupported"
                if v in ("-i", "-o", "-e"):
                    i += 2
                elif v.startswith("-") and v != "--":
                    i += 1
                elif v == "--":
                    i += 1
                    break
                else:
                    break
            inv.external = True
            return args[i:]
        if base == "env":
            i = 0
            while i < len(vals):
                v = vals[i]
                if v is None:
                    return "unsupported"
                if v in ("-i", "-", "--ignore-environment"):
                    inv.envops.append(("clear",))
                    inv.problems.append("env clears the environment")
                    i += 1
                elif v in ("-u", "--unset"):
                    if i + 1 >= len(vals) or vals[i + 1] is None:
                        return "unsupported"
                    inv.envops.append(("unset", vals[i + 1]))
                    i += 2
                elif v.startswith("--unset="):
                    inv.envops.append(("unset", v.split("=", 1)[1]))
                    i += 1
                elif v.startswith("-u") and len(v) > 2:
                    inv.envops.append(("unset", v[2:]))
                    i += 1
                elif v in ("-C", "--chdir"):
                    if i + 1 >= len(vals):
                        return "unsupported"
                    inv.chdirs.append(vals[i + 1])
                    i += 2
                elif v.startswith("--chdir="):
                    inv.chdirs.append(v.split("=", 1)[1])
                    i += 1
                elif v in ("-0", "--null", "-v", "--debug"):
                    i += 1
                elif v == "--":
                    i += 1
                    break
                elif v.startswith("-"):
                    return "unsupported"
                elif "=" in v and NAME_RE.match(v.split("=", 1)[0]):
                    inv.envops.append(("set", v.split("=", 1)[0], v.split("=", 1)[1]))
                    i += 1
                else:
                    break
            while i < len(vals) and vals[i] is not None and "=" in vals[i] and NAME_RE.match(vals[i].split("=", 1)[0]):
                inv.envops.append(("set", vals[i].split("=", 1)[0], vals[i].split("=", 1)[1]))
                i += 1
            inv.external = True
            if i >= len(vals):
                return "data"
            return args[i:]
        if base == "sudo":
            inv.envops.append(("reset",))
            i = 0
            background = False
            while i < len(vals):
                v = vals[i]
                if v is None:
                    return "unsupported"
                if v == "--":
                    i += 1
                    break
                if v in ("-u", "-g", "-C", "-p", "-r", "-t", "-T", "-U", "-h", "-R", "--user", "--group",
                         "--close-from", "--prompt", "--role", "--type", "--command-timeout", "--other-user",
                         "--host", "--chroot"):
                    if v in ("-R", "--chroot"):
                        inv.cwd_unknown = "sudo changes the root directory"
                    i += 2
                elif v in ("-D", "--chdir"):
                    if i + 1 >= len(vals):
                        return "unsupported"
                    inv.chdirs.append(vals[i + 1])
                    i += 2
                elif v.startswith("--chdir="):
                    inv.chdirs.append(v.split("=", 1)[1])
                    i += 1
                elif v in ("-i", "-s", "--login", "--shell"):
                    return "unsupported"
                elif v in ("-l", "--list", "-v", "--validate", "-V", "--version", "-k", "--reset-timestamp",
                           "-K", "--remove-timestamp", "-e", "--edit"):
                    if v in ("-k", "--reset-timestamp"):
                        i += 1
                        continue
                    return "data"
                elif v in ("-b", "--background"):
                    background = True
                    i += 1
                elif v.startswith("--") and "=" in v:
                    i += 1
                elif v.startswith("-") and len(v) > 1 and not v.startswith("--"):
                    letters = v[1:]
                    if any(ch in letters for ch in "isle"):
                        return "unsupported"
                    if any(ch in letters for ch in "ugCprtTUhRD"):
                        return "unsupported"
                    if "b" in letters:
                        background = True
                    i += 1
                elif v.startswith("--"):
                    i += 1
                else:
                    break
            while i < len(vals) and vals[i] is not None and "=" in vals[i] and NAME_RE.match(vals[i].split("=", 1)[0]):
                inv.envops.append(("set", vals[i].split("=", 1)[0], vals[i].split("=", 1)[1]))
                i += 1
            if i < len(vals) and vals[i] is None:
                return "unsupported"
            inv.external = True
            if background:
                inv.background = True
            return args[i:]
        return None

    def effective_cwd(self, st, inv):
        if inv.cwd_unknown or self.remote:
            return None
        cwd = st.cwd
        for d in inv.chdirs:
            if d is None:
                return None
            if os.path.isabs(d):
                cwd = os.path.normpath(d)
            elif cwd is None:
                return None
            else:
                cwd = os.path.normpath(os.path.join(cwd, d))
        return cwd

    def env_problems(self, inv):
        problems = list(inv.problems)
        for frame in self.env_stack + [inv.envops]:
            for op in frame:
                if op[0] in ("set", "unset") and sensitive_env(op[1]):
                    problems.append("it sets or clears " + op[1])
        return problems

    def restart_env(self, inv):
        value = None
        for frame in self.env_stack + [inv.envops]:
            for op in frame:
                if op[0] == "set" and op[1] == "RESTART":
                    value = op[2]
                elif op[0] == "unset" and op[1] == "RESTART":
                    value = None
                elif op[0] in ("clear", "reset"):
                    value = None
        return value == "1"

    def probe_env(self, wrapper_home=None, strip_git=False):
        env = dict(os.environ)
        if strip_git:
            for k in list(env):
                if k.startswith("GIT_") and not (k.startswith("GIT_AUTHOR_") or k.startswith("GIT_COMMITTER_")):
                    del env[k]
            env.pop("XDG_CONFIG_HOME", None)
            if wrapper_home:
                env["HOME"] = wrapper_home
        env.update({"GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
                    "GIT_PAGER": "cat", "PAGER": "cat", "LC_ALL": "C"})
        return env

    def run(self, cwd, args, env, stdin=None):
        key = (cwd, tuple(args), tuple(sorted(env.items())), stdin)
        if key in self.cache:
            return self.cache[key]
        if self.git is None:
            return "no trusted git executable was found"
        if cwd is None or not os.path.isdir(cwd):
            return "the directory %s does not exist" % (cwd,)
        self.probes += 1
        if self.probes > MAX_PROBES:
            return "the command needs too many checks"
        try:
            p = subprocess.run([self.git, "--no-pager", "-c", "core.fsmonitor=false"] + list(args), cwd=cwd,
                               env=env, input=stdin if stdin is not None else "",
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=PROBE_TIMEOUT,
                               universal_newlines=True)
            result = (p.returncode, p.stdout, p.stderr)
        except subprocess.TimeoutExpired:
            result = "a git check timed out"
        except OSError as e:
            result = "a git check could not start (%s)" % e.__class__.__name__
        self.cache[key] = result
        return result

    def resolve_target(self, cwd, opts, env):
        r = self.run(cwd, list(opts) + ["rev-parse", "--path-format=absolute", "--absolute-git-dir",
                                         "--git-common-dir", "--show-toplevel", "--is-shallow-repository"], env)
        if isinstance(r, str):
            return r
        if r[0] != 0:
            return "git could not find a work tree (%s)" % (r[2].strip().splitlines() or ["no detail"])[-1]
        lines = r[1].splitlines()
        if len(lines) != 4:
            return "git returned an unexpected repository description"
        return Target(os.path.realpath(lines[0]), os.path.realpath(lines[1]), os.path.realpath(lines[2]),
                      lines[3].strip() == "true")

    def head_ref(self, cwd, opts, env):
        r = self.run(cwd, list(opts) + ["symbolic-ref", "-q", "HEAD"], env)
        if isinstance(r, str):
            return ("error", r)
        if r[0] == 0:
            return ("ref", r[1].strip())
        if r[0] == 1:
            return ("ref", None)
        return ("error", "git could not read HEAD")

    def identity(self, cwd, opts, env, tgt):
        r = self.run(cwd, list(opts) + ["cat-file", "--batch-check"], env, "\n".join(GO_TRADER_ROOT_COMMITS) + "\n")
        unknown = False
        if isinstance(r, str) or r[0] != 0:
            unknown = True
        else:
            for line in r[1].splitlines():
                if line.strip() and not line.strip().endswith(" missing"):
                    return "go-trader"
        r = self.run(cwd, list(opts) + ["config", "--get-regexp",
                                        r"^(remote\..*\.(url|pushurl)|url\..*\.(insteadof|pushinsteadof))$"], env)
        if isinstance(r, str) or r[0] not in (0, 1):
            unknown = True
        elif r[0] == 0:
            for line in r[1].splitlines():
                if GO_TRADER_REMOTE.search(line):
                    return "go-trader"
        if unknown or tgt.shallow:
            return "unknown"
        return "unrelated"

    def classify_dir(self, path):
        if path is None or not os.path.isdir(path):
            return "unknown"
        env = self.probe_env()
        r = self.run(path, ["rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir",
                            "--show-toplevel", "--is-shallow-repository"], env)
        if isinstance(r, str):
            return "unknown"
        if r[0] != 0:
            if "not a git repository" in r[2]:
                return "none"
            return "unknown"
        lines = r[1].splitlines()
        if len(lines) != 4:
            return "unknown"
        tgt = Target(lines[0], lines[1], lines[2], lines[3].strip() == "true")
        return self.identity(path, [], env, tgt)

    def judge_commit(self, label, st, cwd, opts, env, expect=None):
        tgt = self.resolve_target(cwd, opts, env)
        if isinstance(tgt, str):
            self.deny("%s: the commit target cannot be resolved: %s" % (label, tgt))
            return
        if expect is not None:
            if tgt.gitdir != os.path.realpath(expect[0]) or tgt.top != os.path.realpath(expect[1]):
                self.deny("%s: the wrapper repository record does not match the repository git finds" % label)
                return
        ref = None
        overridden = False
        for gitdir, branch in st.over:
            if gitdir == tgt.gitdir:
                ref = branch
                overridden = True
        if not overridden:
            kind, ref = self.head_ref(cwd, opts, env)
            if kind == "error":
                self.deny("%s: the current branch cannot be read: %s" % (label, ref))
                return
        if ref is not None and ref.startswith("refs/heads/") and ref != PROTECTED_REF:
            return
        ident = self.identity(cwd, opts, env, tgt)
        if ident == "unrelated":
            return
        where = "main" if ref == PROTECTED_REF else "a detached HEAD or a non-branch ref"
        if ident == "go-trader":
            self.deny("%s would commit on %s of go-trader (%s). Commit on a cc/ feature branch in a worktree, "
                      "then open a pull request." % (label, where, tgt.top))
        else:
            self.deny("%s targets %s in %s, and that repository cannot be verified as unrelated to go-trader."
                      % (label, where, tgt.top))

    def commit_preconditions(self, label, st, problems):
        reasons = list(problems)
        if self.remote:
            reasons.append("it runs on a remote host")
        if st.taint:
            reasons.append("an earlier part of this command changes shell state (%s)" % st.taint)
        if self.fs_taint:
            reasons.append("an earlier part of this command can change the branch or target (%s)" % self.fs_taint[0])
        if reasons:
            self.deny("%s cannot be checked: %s. Run the commit as its own command." % (label, "; ".join(reasons)))
            return False
        return True

    def check_commit(self, label, opts, problems, inv, states, conc):
        self.commit_seen = True
        if conc:
            self.concurrent_commit = True
        problems = list(problems) + self.env_problems(inv)
        for st in states:
            if not self.commit_preconditions(label, st, problems):
                continue
            cwd = self.effective_cwd(st, inv)
            if cwd is None:
                self.deny("%s cannot be checked: its working directory is not literal. Run it as its own command "
                          "from the target directory." % label)
                continue
            self.judge_commit(label, st, cwd, opts, self.probe_env())
        return set(states), set(states)

    def do_action_wrapper(self, head, inv, states, conc):
        label = "the action commit wrapper"
        self.commit_seen = True
        if conc:
            self.concurrent_commit = True
        pv = head.value
        problems = list(inv.problems)
        for st in states:
            if not self.commit_preconditions(label, st, problems):
                continue
            if pv is None or "/" not in pv:
                self.deny("%s must be called by a literal path." % label)
                continue
            cwd = self.effective_cwd(st, inv)
            if os.path.isabs(pv):
                path = pv
            elif cwd is None:
                self.deny("%s cannot be checked: its working directory is not literal." % label)
                continue
            else:
                path = os.path.join(cwd, pv)
            d = os.path.dirname(path)
            bad = False
            for name, digest in sorted(ACTION_WRAPPER_SHA256.items()):
                try:
                    with open(os.path.join(d, name), "rb") as fh:
                        got = hashlib.sha256(fh.read()).hexdigest()
                except OSError:
                    got = None
                if got != digest:
                    bad = True
            if bad:
                self.deny("%s at %s is not the reviewed claude-code-action wrapper, so its target is unknown."
                          % (label, path))
                continue
            meta = os.path.join(d, ".git-repo")
            try:
                if os.path.islink(meta) or not os.path.isfile(meta):
                    raise OSError("missing")
                with open(meta, "r") as fh:
                    lines = fh.read().split("\n")
            except (OSError, UnicodeDecodeError):
                self.deny("%s has no readable repository record (.git-repo), so its target is unknown." % label)
                continue
            gitdir = lines[0] if lines else ""
            worktree = lines[1] if len(lines) > 1 else ""
            home = lines[2] if len(lines) > 2 else ""
            if not (gitdir and worktree and os.path.isabs(gitdir) and os.path.isabs(worktree)):
                self.deny("%s has an inconsistent repository record (.git-repo)." % label)
                continue
            opts = ["-c", "safe.bareRepository=explicit", "--git-dir=" + gitdir, "--work-tree=" + worktree]
            env = self.probe_env(wrapper_home=home or None, strip_git=True)
            self.judge_commit(label, st, cwd if cwd and os.path.isdir(cwd) else "/", opts, env,
                              expect=(gitdir, worktree))
        return set(states), set(states)

    def git_global(self, args):
        opts, problems, aliases = [], [], {}
        i = 0
        while i < len(args):
            a = args[i]
            v = a.value
            if v is None:
                return opts, problems, aliases, i, "dynamic"
            if not v.startswith("-"):
                return opts, problems, aliases, i, "ok"
            if v in ("-C", "--git-dir", "--work-tree", "-c", "--namespace", "--attr-source", "--config-env"):
                if i + 1 >= len(args):
                    return opts, problems, aliases, len(args), "info"
                nv = args[i + 1].value
                if nv is None:
                    problems.append("the git option " + v + " is not literal")
                elif v in ("-C", "--git-dir", "--work-tree"):
                    opts += [v, nv]
                elif v == "-c":
                    self.git_config_opt(nv, problems, aliases)
                elif v == "--config-env":
                    self.git_config_opt(nv, problems, None)
                elif v == "--namespace":
                    problems.append("it uses --namespace")
                i += 2
                continue
            if v.startswith("--git-dir=") or v.startswith("--work-tree="):
                opts.append(v)
            elif v.startswith("--config-env="):
                self.git_config_opt(v.split("=", 1)[1], problems, None)
            elif v.startswith("--namespace="):
                problems.append("it uses --namespace")
            elif v == "--bare":
                problems.append("it uses --bare")
            elif v in GIT_INFO_OPTS:
                return opts, problems, aliases, len(args), "info"
            elif v in GIT_FLAG_OPTS or v.startswith("--exec-path=") or v.startswith("--attr-source=") \
                    or v.startswith("--list-cmds=") or v.startswith("--super-prefix="):
                pass
            else:
                problems.append("it uses the unknown git option " + v)
            i += 1
        return opts, problems, aliases, i, "none"

    def git_config_opt(self, text, problems, aliases):
        key = text.split("=", 1)[0].lower()
        for bad in GIT_DENY_CONFIG:
            if key == bad or (bad.endswith(".") and key.startswith(bad)):
                problems.append("it overrides " + key)
        if aliases is not None and key.startswith("alias."):
            aliases[key[6:]] = text.split("=", 1)[1] if "=" in text else ""

    def add_taint(self, reason):
        self.fs_taint.append(reason)

    def do_git(self, args, inv, states, conc):
        opts, problems, aliases, i, status = self.git_global(args)
        if status in ("info", "none"):
            if any(a.value == "commit" for a in args):
                self.check_commit("git commit", opts, problems or ["git options could not be parsed"], inv,
                                  states, conc)
            return set(states), set(states)
        if status == "dynamic":
            self.add_taint("a git subcommand that is not literal")
            return self.check_commit("a git command whose subcommand is not literal", opts, problems, inv,
                                     states, conc)
        return self.git_sub(args[i].value, args[i + 1:], opts, problems, aliases, inv, states, conc)

    def git_sub(self, sub, rest, opts, problems, aliases, inv, states, conc):
        if sub == "commit":
            label = "git %s (an alias for commit)" % self.alias_stack[0] if self.alias_stack else "git commit"
            return self.check_commit(label, opts, problems, inv, states, conc)
        if sub in ("checkout", "switch"):
            return self.git_switch(sub, rest, opts, problems, inv, states, conc)
        if sub in GIT_ALWAYS_TAINT:
            self.add_taint("git " + sub)
            return set(states), set(states)
        if sub == "branch":
            if any(a.value is None or a.value in ("-m", "-M", "--move") or
                   (a.value.startswith("-") and not a.value.startswith("--") and ("m" in a.value or "M" in a.value))
                   for a in rest):
                self.add_taint("git branch rename")
            return set(states), set(states)
        if sub == "config":
            vals = [a.value for a in rest]
            readonly = any(v in ("--get", "--get-all", "--get-regexp", "--get-urlmatch", "-l", "--list", "get",
                                 "list", "--show-origin") for v in vals if v is not None)
            if not readonly or any(v is None for v in vals) or any(v in ("set", "unset", "--unset", "--add",
                                                                          "--replace-all", "--unset-all",
                                                                          "rename-section", "remove-section",
                                                                          "edit", "-e", "--edit")
                                                                     for v in vals):
                self.add_taint("git config")
            return set(states), set(states)
        if sub == "symbolic-ref":
            positional = [a for a in rest if a.value is None or not a.value.startswith("-")]
            if len(positional) != 1 or any(a.value in ("-d", "--delete") for a in rest):
                self.add_taint("git symbolic-ref")
            return set(states), set(states)
        if sub == "worktree":
            action = rest[0].value if rest else None
            if action not in ("list", "prune", "add", "lock", "unlock"):
                self.add_taint("git worktree")
            return set(states), set(states)
        if sub in GIT_SAFE:
            return set(states), set(states)
        return self.git_alias(sub, rest, opts, problems, aliases, inv, states, conc)

    def alias_unknown(self, label, why, opts, problems, inv, states, conc):
        result = self.check_commit("%s (%s)" % (label, why), opts, problems, inv, states, conc)
        self.add_taint(label)
        return result

    def git_alias(self, sub, rest, opts, problems, aliases, inv, states, conc):
        label = "git " + (self.alias_stack[0] if self.alias_stack else sub)
        if sub.lower() in self.alias_stack or len(self.alias_stack) >= MAX_DEPTH:
            return self.alias_unknown(label, "an alias chain that the policy cannot follow", opts, problems, inv,
                                      states, conc)
        groups = {}
        unread = set()
        for st in states:
            if sub.lower() in aliases:
                groups.setdefault(aliases[sub.lower()], set()).add(st)
                continue
            r = self.run(self.effective_cwd(st, inv), list(opts) + ["config", "--get", "alias." + sub],
                         self.probe_env())
            if isinstance(r, str) or r[0] not in (0, 1):
                unread.add(st)
            else:
                groups.setdefault(r[1].strip() if r[0] == 0 else None, set()).add(st)
        succ, fail = set(), set()
        if unread:
            s, f = self.alias_unknown(label, "an alias that cannot be read", opts, problems, inv, unread, conc)
            succ |= s
            fail |= f
        self.alias_stack.append(sub.lower())
        try:
            for value in sorted(groups, key=lambda v: (v is None, v or "")):
                s, f = self.alias_expand(label, value, rest, opts, problems, aliases, inv, groups[value], conc)
                succ |= s
                fail |= f
        finally:
            self.alias_stack.pop()
        return succ, fail

    def alias_expand(self, label, value, rest, opts, problems, aliases, inv, states, conc):
        if value is None:
            return self.alias_unknown(label, "not a known git subcommand or alias", opts, problems, inv, states,
                                      conc)
        if value.startswith("!"):
            return self.shell_alias(label, value[1:], rest, opts, problems, inv, states, conc)
        try:
            parts = shlex.split(value)
        except ValueError:
            return self.alias_unknown(label, "an alias that cannot be parsed", opts, problems, inv, states, conc)
        if not parts:
            return set(states), set(states)
        words = [literal_word(p) for p in parts]
        opts2, problems2, aliases2, j, status = self.git_global(words)
        if opts2 or problems2 or aliases2 or status == "dynamic":
            return self.alias_unknown(label, "an alias with git options", opts, problems, inv, states, conc)
        if status in ("info", "none"):
            if any(a.value == "commit" for a in words + list(rest)):
                return self.alias_unknown(label, "an alias with git options", opts, problems, inv, states, conc)
            return set(states), set(states)
        return self.git_sub(words[j].value, words[j + 1:] + list(rest), opts, problems, aliases, inv, states, conc)

    def shell_alias(self, label, body, rest, opts, problems, inv, states, conc):
        vals = [a.value for a in rest]
        if problems or any(o.startswith(("--git-dir", "--work-tree")) for o in opts) \
                or any(v is None for v in vals) or not body.strip():
            return self.alias_unknown(label, "a shell alias", opts, problems, inv, states, conc)
        text = body + "".join(" " + shlex.quote(v) for v in vals)
        succ, fail = set(), set()
        for st in states:
            cwd = self.effective_cwd(st, inv)
            tgt = self.resolve_target(cwd, opts, self.probe_env()) if cwd else "unknown directory"
            if isinstance(tgt, str):
                s, f = self.alias_unknown(label, "a shell alias", opts, problems, inv, {st}, conc)
            else:
                nested = Inv()
                nested.envops = list(inv.envops)
                nested.problems = list(inv.problems)
                s, f = self.run_nested(literal_word(text), nested, {st._replace(cwd=tgt.top)}, conc, False)
                s = {st._replace(over=r.over) for r in s}
                f = {st._replace(over=r.over) for r in f}
            succ |= s
            fail |= f
        return succ, fail

    def switch_target(self, sub, rest):
        vals = [a.value for a in rest]
        if any(v is None for v in vals):
            return ("unknown",)
        new = None
        detach = False
        track = False
        positional = []
        dashdash = False
        i = 0
        create = ("-b", "-B", "--orphan") if sub == "checkout" else ("-c", "-C", "--create", "--force-create",
                                                                       "--orphan")
        while i < len(vals):
            v = vals[i]
            if dashdash:
                positional.append(v)
            elif v == "--":
                dashdash = True
                if sub == "checkout" and new is None and not detach and i + 1 < len(vals):
                    return ("none",)
            elif v in create:
                if i + 1 >= len(vals):
                    return ("unknown",)
                new = vals[i + 1]
                i += 1
            elif sub == "checkout" and len(v) > 2 and v[:2] in ("-b", "-B"):
                new = v[2:]
            elif v.startswith("--orphan=") or v.startswith("--create=") or v.startswith("--force-create="):
                new = v.split("=", 1)[1]
            elif v in ("--detach", "-d") and (sub == "switch" or v == "--detach"):
                detach = True
            elif v in ("-p", "--patch", "--ours", "--theirs") and sub == "checkout":
                return ("none",)
            elif v in ("-t", "--track") or v.startswith("--track="):
                track = True
            elif v in ("--no-track", "-f", "--force", "-q", "--quiet", "-m", "--merge",
                       "--guess", "--no-guess", "--discard-changes", "--progress", "--no-progress",
                       "--ignore-other-worktrees", "--overwrite-ignore", "--no-overwrite-ignore",
                       "--recurse-submodules", "--no-recurse-submodules", "-l") or v.startswith("--conflict="):
                pass
            elif v.startswith("-"):
                return ("unknown",)
            else:
                positional.append(v)
            i += 1
        if new is not None:
            if "@{" in new:
                return ("unknown",)
            return ("branch", "refs/heads/" + new)
        if detach:
            return ("branch", None)
        if not positional:
            return ("none",)
        if sub == "switch":
            if track or positional[0] == "-" or positional[0].startswith("@") or "@{" in positional[0]:
                return ("unknown",)
            return ("branch", "refs/heads/" + positional[0])
        if len(positional) == 1 and positional[0] == "main":
            return ("branch", PROTECTED_REF)
        if len(positional) >= 2:
            return ("none",)
        return ("unknown",)

    def git_switch(self, sub, rest, opts, problems, inv, states, conc):
        target = self.switch_target(sub, rest)
        if target[0] == "none":
            return set(states), set(states)
        self.branch_changes.append("git " + sub)
        if target[0] == "unknown" or conc or problems or self.remote or self.env_problems(inv):
            self.add_taint("git " + sub)
            return set(states), set(states)
        succ, fail = set(), set()
        for st in states:
            cwd = self.effective_cwd(st, inv)
            tgt = self.resolve_target(cwd, opts, self.probe_env()) if cwd else "unknown directory"
            if isinstance(tgt, str):
                self.add_taint("git " + sub)
                return set(states), set(states)
            over = frozenset([p for p in st.over if p[0] != tgt.gitdir] + [(tgt.gitdir, target[1])])
            succ.add(st._replace(over=over))
            fail.add(st)
        return succ, fail

    def do_update(self, script, args, inv, states, via_shell):
        sim = simulate_update_args([a.value for a in args])
        if sim["kind"] == "help":
            return set(states), set(states)
        restart = sim["restart"] or self.restart_env(inv)
        if restart:
            return set(states), set(states)
        if sim["kind"] == "parsed" and sim["clear"] and not sim["all"] and not sim["rsync"]:
            return set(states), set(states)
        detail = "its arguments are not literal" if sim["kind"] == "unknown" else "restart is not enabled"
        sv = script.value
        for st in states:
            cwd = self.effective_cwd(st, inv)
            path = None
            if sv is not None and cwd is not None:
                if "/" in sv:
                    path = sv if os.path.isabs(sv) else os.path.join(cwd, sv)
                elif via_shell and os.path.isfile(os.path.join(cwd, sv)):
                    path = os.path.join(cwd, sv)
            elif sv is not None and os.path.isabs(sv):
                path = sv
            if path is not None and os.path.isfile(path):
                script_cls = self.classify_dir(os.path.dirname(os.path.realpath(path)))
            else:
                script_cls = "unknown"
            cwd_cls = self.classify_dir(cwd) if cwd is not None and st.taint is None else "unknown"
            if script_cls in ("unrelated", "none") and cwd_cls in ("unrelated", "none"):
                continue
            self.deny("a manual go-trader host update must restart, but %s. Run bash scripts/update.sh --restart "
                      "(or RESTART=1 bash scripts/update.sh). --help and a lone --clear-recovery stay allowed."
                      % detail)
        return set(states), set(states)

    def do_shell(self, args, inv, redirs, states, conc, words):
        vals = [a.value for a in args]
        i = 0
        want_c = False
        bash_syntax_flags = os.path.basename(words[0].value) == "bash"
        noexec = False
        while i < len(vals):
            v = vals[i]
            if v is None:
                if want_c:
                    break
                return self.unsupported(words, states, "a shell option is not literal", True)
            if v == "--" or v == "-":
                bash_syntax_flags = bash_syntax_flags and v == "--"
                i += 1
                break
            if v.startswith(("-", "+")) and len(v) > 1:
                bash_syntax_flags = bash_syntax_flags and bool(BASH_NOEXEC_FLAGS.fullmatch(v))
                noexec = noexec or "n" in v[1:]
            if v in ("-o", "+o", "-O", "+O", "--rcfile", "--init-file"):
                i += 2
                continue
            if v.startswith("--"):
                i += 1
                continue
            if v[:1] in ("-", "+") and len(v) > 1:
                if v[0] == "-" and "c" in v[1:]:
                    want_c = True
                if "o" in v[1:] or "O" in v[1:]:
                    i += 1
                i += 1
                continue
            break
        if want_c:
            if i >= len(vals):
                return set(states), set(states)
            return self.run_nested(args[i], inv, states, conc, False)
        if i < len(vals):
            script = args[i]
            sv = script.value
            if sv is None:
                return self.unsupported(words, states, "the script path is not literal")
            base = os.path.basename(sv)
            if base == "update.sh":
                if bash_syntax_flags and noexec:
                    return set(states), set(states)
                return self.do_update(script, args[i + 1:], inv, states, True)
            if base == "git-commit.sh":
                return self.do_action_wrapper(script, inv, states, conc)
            return set(states), set(states)
        for r in redirs:
            if r["op"] in ("<<", "<<-") and r["body"] is not None:
                fake = Word()
                fake.text = [r["body"]]
                fake.raw = r["body"]
                return self.run_nested(fake, inv, states, conc, False)
            if r["op"] == "<<<":
                return self.run_nested(r["word"], inv, states, conc, False)
            if r["op"] == "<" and r["fd"] in (None, "0"):
                rv = r["word"].value
                if rv is None or os.path.basename(rv) in ("update.sh", "git-commit.sh"):
                    if rv is not None and os.path.basename(rv) == "update.sh":
                        return self.do_update(r["word"], [], inv, states, True)
                    return self.unsupported([r["word"]], states, "a script read from standard input")
                return set(states), set(states)
        return self.unsupported(words, states, "a shell that reads commands from a pipe or standard input", True)

    def run_nested(self, word, inv, states, conc, remote):
        text = word.value
        if text is None:
            return self.unsupported([word], states, "a shell command string that is not literal", True)
        try:
            tree = Parser(text, 1, self.ctx).parse()
        except ParseError as e:
            if UNPARSED_GUARD.search(text):
                self.deny("a nested shell command could not be parsed (%s) and it contains a guarded call" % e)
            return set(states), set(states)
        self.env_stack.append([("reset",)] if remote else list(inv.envops))
        if remote:
            self.remote += 1
        try:
            if remote:
                self.eval_list(tree, {State(None, None, frozenset())}, conc)
                return set(states), set(states)
            succ, fail = set(), set()
            for st in states:
                s, f = self.eval_list(tree, {st._replace(cwd=self.effective_cwd(st, inv))}, conc)
                succ |= {st._replace(over=r.over) for r in s}
                fail |= {st._replace(over=r.over) for r in f}
            return succ, fail
        finally:
            self.env_stack.pop()
            if remote:
                self.remote -= 1

    def do_ssh(self, args, inv, states, conc, words):
        vals = [a.value for a in args]
        i = 0
        while i < len(vals):
            v = vals[i]
            if v is None:
                return self.unsupported(words, states, "an ssh option is not literal", True)
            if v == "--":
                i += 1
                break
            if v.startswith("-") and len(v) > 1:
                if v[-1] in SSH_OPERAND and len(v) == 2:
                    i += 2
                else:
                    i += 1
                continue
            break
        cmd = args[i + 1:]
        if not cmd:
            return set(states), set(states)
        if any(a.value is None for a in cmd):
            return self.unsupported(words, states, "the remote command is not literal", True)
        joined = Word()
        joined.text = [" ".join(a.value for a in cmd)]
        joined.raw = joined.text[0]
        return self.run_nested(joined, inv, states, conc, True)

    def do_source(self, args, inv, states, conc, words):
        taint = "source"
        result = (set(states), set(states))
        if args:
            sv = args[0].value
            if sv is None:
                result = self.unsupported(words, states, "the sourced path is not literal")
            elif os.path.basename(sv) == "update.sh":
                result = self.do_update(args[0], args[1:], inv, states, True)
            elif os.path.basename(sv) == "git-commit.sh":
                result = self.do_action_wrapper(args[0], inv, states, conc)
        tainted = {st._replace(taint=st.taint or taint) for st in result[0] | result[1]}
        return tainted, set(tainted)

    def do_cd(self, name, args, states):
        vals = [a.value for a in args]
        target = ""
        if name == "popd":
            target = None
        else:
            while vals and vals[0] is not None and vals[0].startswith("-") and vals[0] not in ("-", "--"):
                vals.pop(0)
            if vals and vals[0] == "--":
                vals.pop(0)
            if len(vals) > 1:
                target = None
            elif not vals:
                target = os.environ.get("HOME") if name != "pushd" else None
            else:
                target = vals[0]
                if target is None or target == "-" or (name == "pushd" and target[:1] in ("+", "-")):
                    target = None
                elif not os.path.isabs(target) and os.environ.get("CDPATH") and not target.startswith("."):
                    target = None
        succ, fail = set(), set()
        for st in states:
            if target is None:
                succ.add(st._replace(cwd=None))
                fail.add(st)
                continue
            if os.path.isabs(target):
                new = os.path.normpath(target)
            elif st.cwd is None:
                new = None
            else:
                new = os.path.normpath(os.path.join(st.cwd, target))
            succ.add(st._replace(cwd=new))
            if new is None or not (os.path.isdir(new) and os.access(new, os.X_OK)):
                fail.add(st)
        return succ, fail

    def do_shell_state(self, name, args, states, words):
        reason = None
        vals = [a.value for a in args]
        if name in ("export", "declare", "typeset", "local", "readonly"):
            for v in vals:
                if v is None:
                    reason = name + " with a value that is not literal"
                elif v.startswith("-") or v.startswith("+"):
                    if name == "export" or "x" in v or "f" in v:
                        reason = name + " " + v
                elif sensitive_env(v.split("=", 1)[0]):
                    reason = name + " " + v.split("=", 1)[0]
        elif name == "unset":
            for v in vals:
                if v is None or v.startswith("-f") or sensitive_env(v) or v == "-f":
                    reason = "unset " + (v or "a name that is not literal")
        elif name == "set":
            i = 0
            while i < len(vals):
                v = vals[i]
                if v in ("-o", "+o") and i + 1 < len(vals) and vals[i + 1] in SET_OPTIONS_OK:
                    i += 2
                    continue
                if v not in SET_FLAGS_OK:
                    reason = "set " + (v or "with a value that is not literal")
                    break
                i += 1
        elif name in ("shopt", "setopt", "unsetopt", "trap", "hash"):
            reason = None
        else:
            reason = name
            hazards = self.word_hazards(words)
            if name == "eval":
                hazards |= self.text_hazards(" ".join("".join(a.text) for a in args))
                if any(a.value is None for a in args) and self.dynamic_guard():
                    self.deny("eval runs text that is not literal, and the call mentions git, the update script or "
                              "the action commit wrapper. Run the commit or the update as its own literal command.")
            elif name == "alias":
                for v in vals:
                    if v and "=" in v:
                        hazards |= self.text_hazards(v.split("=", 1)[1])
            if "guarded" in hazards:
                self.deny("a guarded call appears inside %s, which the policy cannot check" % name)
            self.note_unchecked(hazards, name)
        if reason:
            states = {st._replace(taint=st.taint or reason) for st in states}
        return set(states), set(states)

    def do_gh(self, args):
        vals = [a.value for a in args[:3]]
        if gh_switches(vals):
            self.add_taint("gh " + " ".join(v for v in vals[:2] if v))


def evaluate(command, cwd):
    ctx = Ctx()
    try:
        tree = Parser(command, 0, ctx).parse()
    except ParseError as e:
        if UNPARSED_GUARD.search(command):
            return ["the command could not be parsed (%s) and it contains a guarded call. Run the commit or the "
                    "update as its own plain command." % e]
        return []
    an = Analyzer(ctx, command)
    an.eval_list(tree, {State(cwd, None, frozenset())}, False)
    if an.concurrent_commit and (an.fs_taint or an.branch_changes):
        an.deny("a commit runs in a pipeline or in the background next to %s." % (an.fs_taint + an.branch_changes)[0])
    if ctx.complex:
        hazards = set()
        for words in ctx.commands:
            hazards |= an.word_hazards(words)
        if "guarded" in hazards:
            an.deny("a guarded call appears inside a shell construct (%s) that the policy cannot check. Run the "
                    "commit or the update as its own plain command." % ", ".join(sorted(set(ctx.complex))))
        an.note_unchecked(hazards, "a shell construct")
    for text in ctx.opaque:
        hazards = an.text_hazards(text)
        if "guarded" in hazards:
            an.deny("a guarded call appears inside an expansion that the policy cannot check.")
        an.note_unchecked(hazards, "an expansion")
    if an.commit_seen and an.unchecked_branch:
        an.deny("a commit runs in the same command as a branch or target change that the policy cannot follow "
                "(inside %s). Run the commit as its own command." % an.unchecked_branch[0])
    return an.denials


def block(message):
    sys.stderr.write("go-trader command policy blocked this Bash call: " + message +
                     "\nPolicy: SKILL.md section Claude Code command policy.\n")
    sys.stderr.flush()
    sys.exit(2)


def main():
    try:
        data = sys.stdin.buffer.read(MAX_INPUT_BYTES + 1)
    except OSError:
        block("the hook input could not be read.")
    if len(data) > MAX_INPUT_BYTES:
        block("the hook input is too large.")
    try:
        payload = json.loads(data.decode("utf-8"))
    except (ValueError, UnicodeDecodeError):
        block("the hook input is not valid JSON.")
    if not isinstance(payload, dict):
        block("the hook input is not a JSON object.")
    event = payload.get("hook_event_name")
    if event is not None and event != "PreToolUse":
        return 0
    tool = payload.get("tool_name")
    if not isinstance(tool, str):
        block("the hook input has no tool name.")
    if tool != "Bash":
        return 0
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict) or not isinstance(tool_input.get("command"), str):
        block("the hook input has no command text.")
    command = tool_input["command"]
    cwd = payload.get("cwd")
    cwd = os.path.normpath(cwd) if isinstance(cwd, str) and os.path.isabs(cwd) else None
    try:
        reasons = evaluate(command, cwd)
    except Exception as e:
        if UNPARSED_GUARD.search(command):
            block("the policy check failed (%s) on a command that can contain a guarded call." % e.__class__.__name__)
        return 0
    if reasons:
        block(" ".join(reasons))
    return 0


if __name__ == "__main__":
    sys.exit(main())
