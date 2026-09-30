#!/usr/bin/env python3
"""Move one hand-made go-trader systemd service to the standard template layout.

Optional and operator-run: scripts/update.sh never calls it. Run the
subcommands as root.

  plan     --unit <old> --instance <name>
           Read-only. Inspects the running unit, its config, environment,
           binary, databases and the shared host files, prints every mapping
           and change, and exits with a refusal code when the move is unsafe.
  apply    --unit <old> --instance <name> [--confirm-live <old>]
           [--plan-id <id>] [--exec-timeout <seconds>]
           Copies the deployment to /opt/go-trader-<name>, moves the config to
           /var/lib/go-trader/<name>/config.json, stops, disables and masks the
           old unit, transfers every state file under both lock types, proves
           the transfer, then installs and starts go-trader@<name>.service and
           proves health, strategy execution and state persistence. Any failure
           recovers automatically.
  rollback --instance <name> [--confirm-live <old>]
           Returns the latest target records to the original paths and restarts
           the original unit. Also recovers an interrupted apply.
  status   [--instance <name>]
           Read-only. Prints the journal and the live state of both units.

Exit status: 0 success; 2 usage; 10-19 refusals before any change (10 unit,
11 config or databases, 12 binary or health contract, 13 environment,
14 target taken, 15 shared host files, 16 target runtime, 17 live
confirmation missing, 18 another migration runs or a transaction is
incomplete, 19 stale plan id); 20 apply failed before the old unit stopped
(nothing changed); 21 apply failed before the new unit started (the old unit
runs again on its own files); 22 apply failed after the new unit started (its
records were returned and the old unit runs again); 30 recovery failed (both
units are held stopped; see the named evidence); 31 rollback refused with no
change.
"""

import argparse
import errno
import glob
import fcntl
import grp
import hashlib
import json
import os
import pwd
import re
import secrets
import shlex
import shutil
import signal
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))
STATE_ROOT = "/var/lib/go-trader/service-layout"
UNIT_DIR = "/etc/systemd/system"
STATE_BASE = "/var/lib/go-trader"
SHARED_STATE_DIR = STATE_BASE + "/shared"
SHARED_FEED_JOURNAL = STATE_BASE + "/shared-feed/convert.journal"
TEMPLATE_NAME = "go-trader@.service"
RESERVED_INSTANCES = {"shared", "shared-feed", "service-layout", "migrate"}
DEFAULT_STATUS_PORT = 8099
HELD_KILL_SWITCH = "portfolio_kill_switch"
DEFAULT_DB_FILE = "scheduler/state.db"
FORBIDDEN_ENV = {"PATH", "UV_CACHE_DIR"}
SYSTEMD_ENV = {
    "LANG", "LANGUAGE", "PATH", "INVOCATION_ID", "JOURNAL_STREAM", "SYSTEMD_EXEC_PID",
    "HOME", "LOGNAME", "USER", "SHELL", "NOTIFY_SOCKET", "MANAGERPID", "MAINPID",
    "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES", "TERM", "RUNTIME_DIRECTORY",
    "STATE_DIRECTORY", "CACHE_DIRECTORY", "LOGS_DIRECTORY", "CONFIGURATION_DIRECTORY",
    "CREDENTIALS_DIRECTORY", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE",
    "WATCHDOG_PID", "WATCHDOG_USEC", "PIDFILE", "MONITOR_EXIT_CODE", "MONITOR_EXIT_STATUS",
    "MONITOR_SERVICE_RESULT", "MONITOR_INVOCATION_ID", "MONITOR_UNIT", "SERVICE_RESULT",
    "EXIT_CODE", "EXIT_STATUS", "TRIGGER_UNIT", "TRIGGER_PATH", "TRIGGER_TIMER_REALTIME_USEC",
    "TRIGGER_TIMER_MONOTONIC_USEC", "LOG_NAMESPACE", "FDSTORE",
}
TERMINAL_STATES = {"aborted", "recovered-source", "rolled-back"}

EXIT_USAGE = 2
EXIT_UNIT = 10
EXIT_CONFIG = 11
EXIT_BINARY = 12
EXIT_ENV = 13
EXIT_TAKEN = 14
EXIT_SHARED = 15
EXIT_RUNTIME = 16
EXIT_CONFIRM = 17
EXIT_BUSY = 18
EXIT_STALE = 19
EXIT_PRESTOP_FAILED = 20
EXIT_SOURCE_RESTORED = 21
EXIT_TARGET_RETURNED = 22
EXIT_RECOVERY_FAILED = 30
EXIT_ROLLBACK_REFUSED = 31

FAIL_AFTER = os.environ.get("MIGRATE_SERVICE_LAYOUT_FAIL_AFTER", "")
KILL_AFTER = os.environ.get("MIGRATE_SERVICE_LAYOUT_KILL_AFTER", "")
PAUSE_AT = os.environ.get("MIGRATE_SERVICE_LAYOUT_PAUSE_AT", "")
STAGE_SEEN = {}

UNIT_SUPPORTED = {
    "Unit": {
        "Description", "Documentation", "After", "Before", "Wants", "Requires",
        "StartLimitIntervalSec", "StartLimitInterval", "StartLimitBurst",
    },
    "Service": {
        "Type", "User", "Group", "WorkingDirectory", "ExecStart", "Environment", "EnvironmentFile",
        "Restart", "RestartSec", "RestartPreventExitStatus", "TimeoutStopSec", "TimeoutStartSec",
        "TimeoutSec", "StandardOutput", "StandardError", "SyslogIdentifier", "KillMode", "KillSignal",
        "LimitNOFILE", "NoNewPrivileges", "PrivateTmp", "ProtectSystem", "ProtectHome",
        "ReadWritePaths", "ReadOnlyPaths", "StateDirectory", "StateDirectoryMode",
        "RuntimeDirectory", "RuntimeDirectoryMode", "LogsDirectory", "LogNamespace", "UMask", "Nice",
    },
    "Install": {"WantedBy"},
}


class Refusal(Exception):
    def __init__(self, code, message):
        Exception.__init__(self, message)
        self.code = code
        self.message = message


class StageFailure(Exception):
    pass


class Interrupted(Exception):
    pass


def log(msg):
    try:
        print("[service-layout] " + msg, flush=True)
    except (BrokenPipeError, OSError):
        pass


def warn(msg):
    try:
        print("[service-layout] WARN " + msg, file=sys.stderr, flush=True)
    except (BrokenPipeError, OSError):
        pass


def die(code, msg):
    try:
        print("[service-layout] ERROR (exit %d) %s" % (code, msg), file=sys.stderr, flush=True)
    except (BrokenPipeError, OSError):
        pass
    sys.exit(code)


def now_iso():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def parse_iso(text):
    if not text:
        return None
    text = text.strip()
    m = re.match(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})?$", text)
    if not m:
        return None
    base = datetime.strptime(m.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
    frac = float("0" + m.group(2)) if m.group(2) else 0.0
    ts = base.timestamp() + frac
    tz = m.group(3)
    if tz and tz != "Z":
        sign = 1 if tz[0] == "+" else -1
        hh, mm = tz[1:].split(":")
        ts -= sign * (int(hh) * 3600 + int(mm) * 60)
    return ts


def run(cmd, check=True, env=None, cwd=None, input_bytes=None, timeout=None):
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, cwd=cwd,
                          input=input_bytes, timeout=timeout,
                          stdin=None if input_bytes is not None else subprocess.DEVNULL)
    if check and proc.returncode != 0:
        raise StageFailure("%s exited %d: %s" % (" ".join(shlex.quote(c) for c in cmd), proc.returncode,
                                                 proc.stderr.decode("utf-8", "replace").strip()[-600:]))
    return proc


def out(cmd, check=True, **kw):
    return run(cmd, check=check, **kw).stdout.decode("utf-8", "replace")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def fingerprint(path):
    if os.path.islink(path) and not os.path.exists(path):
        return "dangling:" + os.readlink(path)
    if not os.path.exists(path):
        return "absent"
    return sha256_file(path)


def db_fingerprint(canon):
    return {
        "db": fingerprint(canon),
        "wal": fingerprint(canon + "-wal") if os.path.exists(canon + "-wal") and os.path.getsize(canon + "-wal") > 0 else "none",
        "journal": "present" if os.path.exists(canon + "-journal") else "none",
    }


def fsync_dir(path):
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_atomic(path, data, mode=0o600, uid=None, gid=None):
    d = os.path.dirname(path)
    fd, tmp = tempfile.mkstemp(prefix=".service-layout-", dir=d)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fchmod(f.fileno(), mode)
            if uid is not None:
                os.fchown(f.fileno(), uid, gid)
            os.fsync(f.fileno())
        os.replace(tmp, path)
        fsync_dir(d)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def copy_file_durable(src, dst, uid=None, gid=None, mode=None):
    d = os.path.dirname(dst)
    fd, tmp = tempfile.mkstemp(prefix=".service-layout-", dir=d)
    try:
        with os.fdopen(fd, "wb") as w, open(src, "rb") as r:
            shutil.copyfileobj(r, w, 1 << 20)
            w.flush()
            st = os.stat(src)
            os.fchmod(w.fileno(), stat.S_IMODE(st.st_mode) if mode is None else mode)
            if uid is not None:
                os.fchown(w.fileno(), uid, gid)
            os.fsync(w.fileno())
        os.replace(tmp, dst)
        fsync_dir(d)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def valid_instance(name):
    if not name or name in (".", "..") or name.startswith("-"):
        return False
    return re.fullmatch(r"[A-Za-z0-9_.-]+", name) is not None


def norm_unit(name):
    return name if name.endswith(".service") else name + ".service"


def within(path, root):
    root = root.rstrip("/")
    return path == root or path.startswith(root + "/")


def canonical_db(wd_real, value):
    p = value if os.path.isabs(value) else os.path.join(wd_real, value)
    p = os.path.normpath(p)
    try:
        os.stat(p)
        return p, os.path.realpath(p)
    except OSError:
        return p, p


def is_memory_db(value):
    v = (value or "").strip()
    return v == "" or v == ":memory:" or ":memory:" in v or "mode=memory" in v


def is_live_args(args):
    args = args or []
    for i, a in enumerate(args):
        if a == "--mode=live":
            return True
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False


def health(port, timeout=5):
    try:
        with urllib.request.urlopen("http://127.0.0.1:%d/health" % port, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            return None
    except Exception:
        return None


def systemctl_show(unit, props):
    cmd = ["systemctl", "show", unit]
    for p in props:
        cmd += ["-p", p]
    text = out(cmd, check=False)
    res = {}
    for line in text.splitlines():
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        res.setdefault(k, []).append(v)
    return {k: (v if k in ("EnvironmentFiles",) else v[-1]) for k, v in res.items()}


def unit_state(unit):
    s = systemctl_show(unit, ["LoadState", "ActiveState", "SubState", "MainPID", "UnitFileState"])
    return {
        "load": s.get("LoadState", ""),
        "active": s.get("ActiveState", ""),
        "sub": s.get("SubState", ""),
        "pid": int(s.get("MainPID", "0") or "0"),
        "file": s.get("UnitFileState", ""),
    }


def daemon_reload():
    run(["systemctl", "daemon-reload"])


def unit_file_directives(unit):
    text = out(["systemctl", "cat", "--no-pager", unit], check=False)
    found = []
    section = ""
    pending = ""
    for raw in text.splitlines():
        line = pending + raw
        pending = ""
        if line.endswith("\\"):
            pending = line[:-1] + " "
            continue
        s = line.strip()
        if not s or s.startswith("#") or s.startswith(";"):
            continue
        if s.startswith("[") and s.endswith("]"):
            section = s[1:-1]
            continue
        if "=" in s:
            k, v = s.split("=", 1)
            found.append((section, k.strip(), v.strip()))
    return found


def parse_execstart(value):
    cmds = re.findall(r"\{ path=(.*?) ; argv\[\]=(.*?) ; ignore_errors=(yes|no) ;", value or "")
    return cmds


def parse_env_assignments(value):
    try:
        items = shlex.split(value or "")
    except ValueError:
        return None
    env = []
    for it in items:
        if "=" not in it:
            return None
        k, v = it.split("=", 1)
        env.append((k, v))
    return env


def parse_env_file(path):
    with open(path, "rb") as f:
        data = f.read().decode("utf-8", "surrogateescape")
    result = []
    i = 0
    n = len(data)
    key_re = re.compile(r"[A-Za-z_][A-Za-z0-9_]*$")
    while i < n:
        while i < n and data[i] in " \t\r\n":
            i += 1
        if i >= n:
            break
        if data[i] in "#;":
            while i < n and data[i] != "\n":
                i += 1
            continue
        start = i
        while i < n and data[i] not in "=\n":
            i += 1
        if i >= n or data[i] == "\n":
            continue
        key = data[start:i].strip()
        i += 1
        while i < n and data[i] in " \t":
            i += 1
        val = []
        while i < n and data[i] != "\n":
            c = data[i]
            if c == "'":
                i += 1
                while i < n and data[i] != "'":
                    val.append(data[i])
                    i += 1
                i += 1
            elif c == '"':
                i += 1
                while i < n and data[i] != '"':
                    if data[i] == "\\" and i + 1 < n:
                        nxt = data[i + 1]
                        if nxt == "\n":
                            i += 2
                            continue
                        if nxt in '"\\`$':
                            val.append(nxt)
                            i += 2
                            continue
                    val.append(data[i])
                    i += 1
                i += 1
            elif c == "\\" and i + 1 < n:
                if data[i + 1] == "\n":
                    i += 2
                    continue
                val.append(data[i + 1])
                i += 2
            else:
                val.append(c)
                i += 1
        value = "".join(val).rstrip(" \t\r")
        if key_re.match(key):
            result.append((key, value))
    return result


def env_file_line(key, value):
    if re.fullmatch(r"[A-Za-z0-9_./:@%+,=-]*", value):
        return "%s=%s\n" % (key, value)
    escaped = value.replace("\\", "\\\\").replace('"', '\\"').replace("`", "\\`").replace("$", "\\$")
    return '%s="%s"\n' % (key, escaped)


def proc_environ(pid):
    try:
        with open("/proc/%d/environ" % pid, "rb") as f:
            raw = f.read()
    except OSError:
        return None
    env = {}
    for item in raw.split(b"\0"):
        if not item or b"=" not in item:
            continue
        k, v = item.split(b"=", 1)
        env[k.decode("utf-8", "surrogateescape")] = v.decode("utf-8", "surrogateescape")
    return env


def manager_environment():
    env = {}
    for line in out(["systemctl", "show-environment"], check=False).splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            env[k] = v
    return env


def user_info(name):
    try:
        pw = pwd.getpwnam(name)
    except KeyError:
        return None
    gids = [g.gr_gid for g in grp.getgrall() if name in g.gr_mem]
    gids.append(pw.pw_gid)
    return {"name": name, "uid": pw.pw_uid, "gid": pw.pw_gid, "gids": sorted(set(gids))}


def other_can(path, need_exec_dirs=True):
    parts = []
    p = os.path.realpath(path)
    cur = p
    while True:
        parts.append(cur)
        parent = os.path.dirname(cur)
        if parent == cur:
            break
        cur = parent
    for comp in parts[1:]:
        try:
            st = os.stat(comp)
        except OSError:
            return False
        if not (st.st_mode & stat.S_IXOTH):
            return False
    try:
        st = os.stat(p)
    except OSError:
        return False
    if stat.S_ISDIR(st.st_mode):
        return bool(st.st_mode & stat.S_IXOTH) and bool(st.st_mode & stat.S_IROTH)
    return bool(st.st_mode & stat.S_IROTH)


def owner_text(owner):
    uid, gid = owner
    try:
        u = pwd.getpwuid(uid).pw_name
    except KeyError:
        u = str(uid)
    try:
        g = grp.getgrgid(gid).gr_name
    except KeyError:
        g = str(gid)
    return "%s:%s" % (u, g)


def as_user_ok(user, argv):
    if user is None:
        return None
    p = run(["runuser", "-u", user, "--"] + argv, check=False, timeout=60)
    return p.returncode == 0


def probe_lock(path):
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return ("absent", 0)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as e:
            if e.errno in (errno.EWOULDBLOCK, errno.EAGAIN):
                raw = b""
                try:
                    raw = os.pread(fd, 32, 0)
                except OSError:
                    pass
                try:
                    pid = int(raw.decode("utf-8", "replace").strip() or "0")
                except ValueError:
                    pid = 0
                return ("held", pid)
            return ("error", 0)
        fcntl.flock(fd, fcntl.LOCK_UN)
        return ("free", 0)
    finally:
        os.close(fd)


class LockSet:
    def __init__(self):
        self.held = []

    def acquire(self, path, uid, gid, write_pid, timeout):
        existed = os.path.exists(path)
        fd = os.open(path, os.O_CREAT | os.O_RDWR, 0o644)
        if not existed:
            try:
                os.fchown(fd, uid, gid)
            except OSError:
                pass
        deadline = time.time() + timeout
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except OSError as e:
                if e.errno not in (errno.EWOULDBLOCK, errno.EAGAIN):
                    os.close(fd)
                    raise StageFailure("flock %s: %s" % (path, e))
                if time.time() >= deadline:
                    raw = b""
                    try:
                        raw = os.pread(fd, 32, 0)
                    except OSError:
                        pass
                    os.close(fd)
                    raise StageFailure("lock %s is held by pid %s" % (path, raw.decode("utf-8", "replace").strip() or "unknown"))
                time.sleep(0.2)
        if write_pid:
            os.ftruncate(fd, 0)
            os.pwrite(fd, ("%d\n" % os.getpid()).encode(), 0)
            os.fsync(fd)
        self.held.append((path, fd))

    def release(self):
        for _path, fd in reversed(self.held):
            try:
                os.close(fd)
            except OSError:
                pass
        self.held = []

    def paths(self):
        return [p for p, _ in self.held]


def acquire_db_locks(lockset, dbs, key, uid_of, timeout=30):
    for db in dbs:
        canon = db[key]
        uid, gid = uid_of(db)
        lockset.acquire(canon + ".lock", uid, gid, True, timeout)
    for db in dbs:
        canon = db[key]
        uid, gid = uid_of(db)
        lockset.acquire(canon + ".manual-action.lock", uid, gid, False, timeout)


def db_digest(path):
    work = tempfile.mkdtemp(prefix="service-layout-digest-")
    try:
        copy = os.path.join(work, "d.db")
        shutil.copyfile(path, copy)
        if os.path.exists(path + "-wal"):
            shutil.copyfile(path + "-wal", copy + "-wal")
        con = sqlite3.connect(copy)
        try:
            ok = con.execute("PRAGMA integrity_check").fetchone()[0]
            if ok != "ok":
                raise StageFailure("integrity_check of %s: %s" % (path, ok))
            schema = sorted((r[0], r[1], r[2] or "") for r in con.execute("SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'"))
            tables = [r[1] for r in schema if r[0] == "table"]
            h = hashlib.sha256()
            h.update(repr(schema).encode())
            counts = {}
            for t in tables:
                rows = sorted(repr(r) for r in con.execute('SELECT * FROM "%s"' % t.replace('"', '""')))
                counts[t] = len(rows)
                th = hashlib.sha256()
                for r in rows:
                    th.update(r.encode("utf-8", "surrogateescape"))
                    th.update(b"\0")
                h.update(t.encode() + b":" + th.hexdigest().encode())
            return {"digest": h.hexdigest(), "counts": counts}
        finally:
            con.close()
    finally:
        shutil.rmtree(work, ignore_errors=True)


def consolidate(src_db, src_wal, out_path):
    work = tempfile.mkdtemp(prefix="service-layout-consolidate-", dir=os.path.dirname(out_path))
    try:
        copy = os.path.join(work, "c.db")
        shutil.copyfile(src_db, copy)
        if src_wal and os.path.exists(src_wal):
            shutil.copyfile(src_wal, copy + "-wal")
        con = sqlite3.connect(copy)
        try:
            con.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchall()
            ok = con.execute("PRAGMA integrity_check").fetchone()[0]
            if ok != "ok":
                raise StageFailure("integrity_check of %s: %s" % (src_db, ok))
        finally:
            con.close()
        for side in ("-wal", "-shm"):
            if os.path.exists(copy + side):
                if side == "-wal" and os.path.getsize(copy + side) > 0:
                    raise StageFailure("checkpoint of %s left a non-empty WAL" % src_db)
                os.unlink(copy + side)
        with open(copy, "rb") as f:
            os.fsync(f.fileno())
        os.replace(copy, out_path)
        fsync_dir(os.path.dirname(out_path))
    finally:
        shutil.rmtree(work, ignore_errors=True)


def git(tree, *args):
    return ["git", "-c", "safe.directory=*", "--no-optional-locks", "-C", tree] + list(args)


CODE_DIRS = ("scheduler", "shared_scripts", "shared_strategies", "shared_tools", "platforms", "scripts", "systemd", "backtest")
CODE_EXT = (".go", ".mod", ".sum", ".py", ".sh", ".service", ".conf", ".js", ".html", ".css")


def tree_code_fingerprint(tree):
    h = hashlib.sha256()
    h.update(b"files\0")
    rels = [f for f in ("pyproject.toml", "uv.lock") if os.path.isfile(os.path.join(tree, f))]
    for d in CODE_DIRS:
        for root, dirs, files in os.walk(os.path.join(tree, d)):
            dirs[:] = [n for n in dirs if n != "__pycache__"]
            for n in files:
                if n.endswith(CODE_EXT):
                    rels.append(os.path.relpath(os.path.join(root, n), tree))
    for rel in sorted(set(rels)):
        full = os.path.join(tree, rel)
        h.update(rel.encode("utf-8", "surrogateescape") + b"\0")
        if os.path.islink(full):
            h.update(b"L" + os.readlink(full).encode("utf-8", "surrogateescape"))
        elif os.path.isfile(full):
            h.update(sha256_file(full).encode())
    return h.hexdigest()


def source_fingerprint(tree):
    if not os.path.isdir(os.path.join(tree, ".git")):
        return tree_code_fingerprint(tree)
    p = run(git(tree, "ls-files", "-z"), check=False)
    if p.returncode != 0:
        return tree_code_fingerprint(tree)
    h = hashlib.sha256()
    head = out(git(tree, "rev-parse", "HEAD"), check=False).strip()
    h.update(head.encode())
    for rel in sorted(x for x in p.stdout.decode("utf-8", "surrogateescape").split("\0") if x):
        full = os.path.join(tree, rel)
        h.update(rel.encode("utf-8", "surrogateescape") + b"\0")
        if os.path.islink(full):
            h.update(b"L" + os.readlink(full).encode("utf-8", "surrogateescape"))
        elif os.path.isfile(full):
            h.update(sha256_file(full).encode())
        else:
            h.update(b"-")
    return h.hexdigest()


def rel_inside(path, root):
    if within(path, root):
        return os.path.relpath(path, root)
    return None


class Plan:
    def __init__(self, unit, instance):
        self.unit = unit
        self.instance = instance
        self.refusals = []
        self.changes = []
        self.notes = []
        self.data = {}

    def refuse(self, code, msg):
        self.refusals.append((code, msg))

    def change(self, msg):
        self.changes.append(msg)

    def note(self, msg):
        self.notes.append(msg)


def template_directives(path):
    res = {}
    with open(path) as f:
        for line in f:
            s = line.strip()
            if "=" in s and not s.startswith("#"):
                k, v = s.split("=", 1)
                res.setdefault(k.strip(), []).append(v.strip())
    return res


def discover_scheduler_units():
    units = set()
    p = run(["systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain",
             "go-trader.service", "go-trader-*.service", "go-trader@*.service"], check=False)
    for line in p.stdout.decode("utf-8", "replace").splitlines():
        parts = line.split()
        if parts:
            units.add(parts[0])
    p = run(["systemctl", "list-unit-files", "--type=service", "--no-legend", "--plain",
             "go-trader.service", "go-trader-*.service", "go-trader@*.service"], check=False)
    for line in p.stdout.decode("utf-8", "replace").splitlines():
        parts = line.split()
        if parts and not parts[0].endswith("@.service"):
            units.add(parts[0])
    return sorted(units)


def config_path_for(unit):
    s = systemctl_show(unit, ["WorkingDirectory", "ExecStart"])
    wd = s.get("WorkingDirectory", "")
    cmds = parse_execstart(s.get("ExecStart", ""))
    cfg = "scheduler/config.json"
    if cmds:
        try:
            argv = shlex.split(cmds[0][1])
        except ValueError:
            argv = []
        for i, a in enumerate(argv):
            if a in ("--config", "-config") and i + 1 < len(argv):
                cfg = argv[i + 1]
            elif a.startswith("--config=") or a.startswith("-config="):
                cfg = a.split("=", 1)[1]
    if not os.path.isabs(cfg):
        if not wd:
            return ""
        cfg = os.path.join(wd, cfg)
    return os.path.realpath(cfg)


def shared_feed_consumers():
    if not os.path.isfile(SHARED_FEED_JOURNAL):
        return set()
    units = set()
    last_set = None
    with open(SHARED_FEED_JOURNAL) as f:
        for line in f:
            line = line.strip()
            if line.startswith("consumers-set "):
                for tok in line.split():
                    if tok.startswith("units="):
                        last_set = set(u for u in tok[len("units="):].split(",") if u)
            elif line.startswith("consumer "):
                parts = line.split()
                if len(parts) > 1:
                    units.add(parts[1])
    return last_set if last_set is not None else units


def inspect(unit, instance, exec_timeout=None):
    plan = Plan(unit, instance)
    d = plan.data
    d["unit"] = unit
    d["instance"] = instance
    target_unit = "go-trader@%s.service" % instance
    tgt_wd = "/opt/go-trader-%s" % instance
    cfg_dir = "%s/%s" % (STATE_BASE, instance)
    d["target_unit"] = target_unit
    d["target_dir"] = tgt_wd
    d["target_config_dir"] = cfg_dir
    d["target_config"] = cfg_dir + "/config.json"

    if "@" in unit:
        plan.refuse(EXIT_UNIT, "%s is a template instance; this tool moves a hand-made (non-template) unit" % unit)
        return plan
    props = ["Id", "LoadState", "ActiveState", "SubState", "UnitFileState", "FragmentPath", "DropInPaths",
             "NeedDaemonReload", "MainPID", "User", "Group", "WorkingDirectory", "ExecStart", "ExecStartEx",
             "Environment", "EnvironmentFiles", "PassEnvironment", "UnsetEnvironment", "Type", "TriggeredBy",
             "RequiredBy", "RequisiteOf", "BoundBy", "ConsistsOf", "UpheldBy", "WantedBy", "PartOf", "Names",
             "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition"]
    s = systemctl_show(unit, props)
    d["show"] = {k: s.get(k, "") for k in ("LoadState", "ActiveState", "UnitFileState", "FragmentPath", "DropInPaths", "User", "Group", "WorkingDirectory", "Type")}
    if s.get("LoadState") != "loaded":
        plan.refuse(EXIT_UNIT, "%s is not a loaded unit (LoadState=%s)" % (unit, s.get("LoadState", "unknown")))
        return plan
    if s.get("ActiveState") != "active":
        plan.refuse(EXIT_UNIT, "%s is %s; start it first, because the move proves the running version, port and environment" % (unit, s.get("ActiveState", "unknown")))
        return plan
    if s.get("NeedDaemonReload") == "yes":
        plan.refuse(EXIT_UNIT, "%s has unit-file changes that systemd has not loaded; run systemctl daemon-reload and restart it first" % unit)
    frag = s.get("FragmentPath", "")
    if not frag or not (frag.startswith(UNIT_DIR + "/") or frag.startswith("/lib/systemd/system/") or frag.startswith("/usr/lib/systemd/system/")):
        plan.refuse(EXIT_UNIT, "%s is loaded from %s; only unit files under %s, /lib or /usr/lib are supported" % (unit, frag or "nowhere", UNIT_DIR))
    names = (s.get("Names") or "").split()
    if len([n for n in names if n != unit]) > 0:
        plan.refuse(EXIT_UNIT, "%s has alias names (%s); aliases are service references this tool cannot move" % (unit, " ".join(n for n in names if n != unit)))
    d["fragment"] = frag
    d["fragment_fp"] = fingerprint(frag) if frag else "absent"
    d["fragment_is_link"] = bool(frag) and os.path.islink(frag)
    dropins = [p for p in (s.get("DropInPaths") or "").split() if p]
    d["dropins"] = dropins
    d["dropin_fps"] = {p: fingerprint(p) for p in dropins}
    d["unit_file_state"] = s.get("UnitFileState", "")
    main_pid = int(s.get("MainPID", "0") or "0")
    d["main_pid"] = main_pid

    for key in ("ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition"):
        if (s.get(key) or "").strip():
            plan.refuse(EXIT_UNIT, "%s sets %s; the template has no equivalent" % (unit, key))
    if s.get("PassEnvironment", "").strip() or s.get("UnsetEnvironment", "").strip():
        plan.refuse(EXIT_ENV, "%s uses PassEnvironment or UnsetEnvironment, which the template cannot preserve" % unit)
    for key in ("TriggeredBy", "RequiredBy", "RequisiteOf", "BoundBy", "ConsistsOf", "UpheldBy"):
        refs = [r for r in (s.get(key) or "").split() if r and not r.endswith(".target")]
        if refs:
            plan.refuse(EXIT_UNIT, "%s is referenced by %s through %s; stopping and masking it would change that unit" % (unit, " ".join(refs), key))
    wanted = [r for r in (s.get("WantedBy") or "").split() if r and not r.endswith(".target")]
    if wanted:
        plan.change("%s pull in %s through Wants=; after the move they pull in a masked unit and must name %s instead" % (" ".join(wanted), unit, target_unit))

    unsupported = []
    reported = set()
    for section, key, value in unit_file_directives(unit):
        allowed = UNIT_SUPPORTED.get(section)
        if allowed is None or key not in allowed:
            unsupported.append("[%s] %s" % (section, key))
            continue
        if section == "Unit" and key in ("After", "Before", "Wants", "Requires"):
            others = [v for v in value.split() if not v.endswith(".target")]
            if others and key in ("Wants", "Requires"):
                unsupported.append("[Unit] %s=%s" % (key, " ".join(others)))
            elif others:
                reported.add("[Unit] %s=%s is not carried; the template orders only After=network.target" % (key, value))
        if section == "Install" and key == "WantedBy":
            if any(not v.endswith(".target") for v in value.split()):
                unsupported.append("[Install] WantedBy=%s" % value)
        if section == "Service" and key == "Type" and value not in ("simple", "exec"):
            unsupported.append("[Service] Type=%s" % value)
        if section == "Service" and key in ("KillMode",) and value not in ("control-group", "mixed"):
            unsupported.append("[Service] KillMode=%s" % value)
        if section == "Service" and key in ("StandardOutput", "StandardError") and value not in ("journal", "inherit", "journal+console"):
            unsupported.append("[Service] %s=%s" % (key, value))
        if section == "Service" and key in ("Restart", "RestartSec", "RestartPreventExitStatus", "TimeoutStopSec", "TimeoutStartSec",
                                            "TimeoutSec", "SyslogIdentifier", "KillMode", "KillSignal", "LimitNOFILE", "NoNewPrivileges",
                                            "PrivateTmp", "ProtectSystem", "ProtectHome", "ReadWritePaths", "ReadOnlyPaths",
                                            "StateDirectory", "StateDirectoryMode", "RuntimeDirectory", "RuntimeDirectoryMode",
                                            "LogsDirectory", "LogNamespace", "UMask", "Nice", "StartLimitIntervalSec"):
            reported.add("%s=%s is replaced by the template's setting" % (key, value))
    if unsupported:
        plan.refuse(EXIT_UNIT, "%s sets directives the template cannot preserve: %s" % (unit, ", ".join(sorted(set(unsupported)))))
    for r in sorted(reported):
        plan.change("unit: " + r)

    wd = s.get("WorkingDirectory", "")
    if not wd or not os.path.isabs(wd) or not os.path.isdir(wd):
        plan.refuse(EXIT_UNIT, "%s has no usable WorkingDirectory (%s)" % (unit, wd or "unset"))
        return plan
    wd_real = os.path.realpath(wd)
    d["source_dir"] = wd
    d["source_dir_real"] = wd_real
    d["tree_mode"] = 0o755 if other_can(wd_real) else 0o700
    if within(wd_real, tgt_wd) or within(tgt_wd, wd_real):
        plan.refuse(EXIT_TAKEN, "the source tree %s overlaps the target %s; choose another instance name" % (wd_real, tgt_wd))
        return plan

    user = s.get("User") or "root"
    group = s.get("Group") or ""
    if not group:
        try:
            group = grp.getgrgid(pwd.getpwnam(user).pw_gid).gr_name
        except KeyError:
            group = user
    d["source_user"] = user
    d["source_group"] = group

    cmds = parse_execstart(s.get("ExecStart", ""))
    if len(cmds) != 1:
        plan.refuse(EXIT_UNIT, "%s must have exactly one ExecStart command (found %d)" % (unit, len(cmds)))
        return plan
    exe_path, argv_text, ignore = cmds[0]
    if ignore != "no":
        plan.refuse(EXIT_UNIT, "%s ExecStart ignores errors ('-' prefix)" % unit)
    ex = s.get("ExecStartEx", "")
    flags = re.findall(r"flags=([^;]*);", ex)
    if any(f.strip() for f in flags):
        plan.refuse(EXIT_UNIT, "%s ExecStart uses prefix flags (%s)" % (unit, ",".join(f.strip() for f in flags)))
    try:
        argv = shlex.split(argv_text)
    except ValueError:
        argv = []
    binary = os.path.join(wd_real, "go-trader")
    if not argv or os.path.realpath(exe_path) != os.path.realpath(binary):
        plan.refuse(EXIT_BINARY, "%s runs %s, not the deployment binary %s" % (unit, exe_path, binary))
        return plan
    cfg_arg = "scheduler/config.json"
    rest = argv[1:]
    i = 0
    extra = []
    while i < len(rest):
        a = rest[i]
        if a in ("--config", "-config") and i + 1 < len(rest):
            cfg_arg = rest[i + 1]
            i += 2
            continue
        if a.startswith("--config=") or a.startswith("-config="):
            cfg_arg = a.split("=", 1)[1]
            i += 1
            continue
        extra.append(a)
        i += 1
    if extra:
        plan.refuse(EXIT_UNIT, "%s passes %s; only --config is supported, move other settings into the config first" % (unit, " ".join(extra)))
    cfg_path = cfg_arg if os.path.isabs(cfg_arg) else os.path.join(wd_real, cfg_arg)
    cfg_real = os.path.realpath(cfg_path)
    d["source_config"] = cfg_path
    d["source_config_real"] = cfg_real
    d["binary"] = binary

    for need in ("go-trader", "scheduler", "shared_scripts", ".venv/bin/python3", "systemd/" + TEMPLATE_NAME,
                 "scripts/install-service.sh", "scripts/update_helpers.sh"):
        if not os.path.exists(os.path.join(wd_real, need)):
            plan.refuse(EXIT_BINARY, "%s is missing from %s; update the deployment with scripts/update.sh --restart first" % (need, wd_real))
    if plan.refusals and any(c == EXIT_BINARY for c, _ in plan.refusals):
        return plan
    d["binary_sha"] = sha256_file(binary)
    exe_sha = ""
    try:
        exe_sha = sha256_file("/proc/%d/exe" % main_pid)
    except OSError:
        pass
    if exe_sha != d["binary_sha"]:
        plan.refuse(EXIT_BINARY, "%s runs a binary that differs from %s (restart pending or updated without restart); restart it first" % (unit, binary))
    with open(binary, "rb") as f:
        blob = f.read()
    for marker in (b"run_evidence", b"storage-inspect", b"zero_capital_skipped", b"portfolio_kill_switch"):
        if marker not in blob:
            plan.refuse(EXIT_BINARY, "the binary %s predates the migration contract (no %s); run scripts/update.sh --restart first" % (binary, marker.decode()))
    d["source_fp"] = source_fingerprint(wd_real)
    if os.path.isdir(os.path.join(wd_real, ".git")):
        d["git_head"] = out(git(wd_real, "rev-parse", "HEAD"), check=False).strip()
        dirty = out(git(wd_real, "status", "--porcelain", "--untracked-files=no"), check=False).strip()
        if dirty:
            plan.note("the source tree has uncommitted changes to tracked files; they are copied as they are")
    else:
        d["git_head"] = ""
        plan.note("the source tree is not a git checkout (an rsync deploy); the copy is proven by checksums and a fingerprint of its code files")

    tmpl_src = os.path.join(wd_real, "systemd", TEMPLATE_NAME)
    tmpl = template_directives(tmpl_src)
    t_user = (tmpl.get("User") or ["go-trader"])[-1]
    t_group = (tmpl.get("Group") or [t_user])[-1]
    if "%" in t_user or "%" in t_group:
        plan.refuse(EXIT_SHARED, "the shipped template sets a templated User/Group; unsupported")
    d["target_user"] = t_user
    d["target_group"] = t_group
    d["template_fp"] = fingerprint(tmpl_src)
    namespace = (tmpl.get("LogNamespace") or [""])[-1]
    d["log_namespace"] = namespace
    rw = " ".join(tmpl.get("ReadWritePaths", [])).replace("%i", instance).split()
    d["template_rw"] = rw
    tuser = user_info(t_user)
    d["target_user_exists"] = tuser is not None
    if tuser is not None:
        try:
            grp.getgrnam(t_group)
        except KeyError:
            plan.refuse(EXIT_RUNTIME, "the account %s exists but the template's group %s does not" % (t_user, t_group))
    if tuser is None:
        plan.change("creates the system account %s:%s" % (t_user, t_group))
    plan.change("service identity: %s:%s -> %s:%s" % (user, group, t_user, t_group))

    if not os.path.isfile(cfg_real):
        plan.refuse(EXIT_CONFIG, "config %s does not exist" % cfg_path)
        return plan
    try:
        with open(cfg_real, "rb") as f:
            cfg_bytes = f.read()
        cfg = json.loads(cfg_bytes.decode("utf-8"))
        if not isinstance(cfg, dict):
            raise ValueError("not an object")
    except Exception as e:
        plan.refuse(EXIT_CONFIG, "config %s does not parse (%s); discovery refuses to guess its databases" % (cfg_path, e))
        return plan
    d["config_fp"] = hashlib.sha256(cfg_bytes).hexdigest()
    st = os.stat(cfg_real)
    d["config_mode"] = stat.S_IMODE(st.st_mode)
    d["config_owner"] = [st.st_uid, st.st_gid]
    role = cfg.get("role")
    if isinstance(role, str) and role.strip() == "feed":
        plan.refuse(EXIT_CONFIG, "%s is a market feed service (role: feed); feeds are managed by scripts/shared-feed-convert.sh" % unit)
    mf = cfg.get("market_feed")
    if isinstance(mf, str) and mf.strip() == "shared":
        plan.refuse(EXIT_CONFIG, "%s consumes the shared market feed; roll it back with scripts/shared-feed-convert.sh first" % unit)
    if unit in shared_feed_consumers():
        plan.refuse(EXIT_CONFIG, "%s is a current shared-feed consumer in %s; drop it from the feeds first" % (unit, SHARED_FEED_JOURNAL))
    strategies = cfg.get("strategies") or []
    if not isinstance(strategies, list):
        plan.refuse(EXIT_CONFIG, "config strategies is not a list")
        strategies = []
    live = any(isinstance(sc, dict) and (is_live_args(sc.get("args")) or sc.get("type") == "manual") for sc in strategies)
    d["live"] = live
    d["strategy_ids"] = sorted(str(sc.get("id")) for sc in strategies if isinstance(sc, dict))
    port = cfg.get("status_port") or DEFAULT_STATUS_PORT
    if not isinstance(port, int) or isinstance(port, bool):
        plan.refuse(EXIT_CONFIG, "status_port is not an integer")
        port = DEFAULT_STATUS_PORT
    d["status_port"] = port
    intervals = [cfg.get("interval_seconds") or 600]
    for sc in strategies:
        if isinstance(sc, dict) and isinstance(sc.get("interval_seconds"), int) and sc.get("interval_seconds") > 0:
            intervals.append(sc["interval_seconds"])
    d["exec_timeout"] = int(exec_timeout) if exec_timeout else max(300, max(i for i in intervals if isinstance(i, int)) + 300)

    h = health(port)
    if not h or h.get("pid") != main_pid:
        plan.refuse(EXIT_BINARY, "status port %d does not answer /health with the unit's pid %d (got %s); the effective port is unknown" % (port, main_pid, (h or {}).get("pid", "no answer")))
    else:
        d["version"] = h.get("version", "")
        rev = h.get("run_evidence")
        if not isinstance(rev, dict) or not isinstance(rev.get("held"), dict):
            plan.refuse(EXIT_BINARY, "the running daemon's /health has no run_evidence with held strategies; run scripts/update.sh --restart first")
        else:
            by_reason = {}
            evaluated_at = rev.get("evaluated") if isinstance(rev.get("evaluated"), dict) else {}
            for sid, hold in sorted(rev["held"].items()):
                if not isinstance(hold, dict):
                    continue
                if (parse_iso(evaluated_at.get(sid)) or 0) > (parse_iso(hold.get("at")) or 0):
                    continue
                by_reason.setdefault(hold.get("reason") or "unknown", []).append(sid)
            if by_reason.get(HELD_KILL_SWITCH):
                plan.note("the portfolio kill switch of the running daemon held %s in its latest cycle; the execution proof accepts a strategy the target holds the same way" % ", ".join(by_reason[HELD_KILL_SWITCH]))
            for reason in sorted(r for r in by_reason if r != HELD_KILL_SWITCH):
                plan.note("the running daemon held %s (%s) in its latest cycle; the execution proof fails at once if the target holds a strategy this way" % (", ".join(by_reason[reason]), reason))

    rewrites = []
    dbs = []
    fields = [("primary", "db_file", cfg.get("db_file") if isinstance(cfg.get("db_file"), str) and cfg.get("db_file").strip() else DEFAULT_DB_FILE, ["db_file"])]
    pdb = cfg.get("paper_db_file")
    if pdb is not None and not isinstance(pdb, str):
        plan.refuse(EXIT_CONFIG, "paper_db_file is not a string")
    if isinstance(pdb, str) and pdb.strip():
        fields.append(("paper", "paper_db_file", pdb, ["paper_db_file"]))
    sources = cfg.get("paper_sources") or []
    if not isinstance(sources, list):
        plan.refuse(EXIT_CONFIG, "paper_sources is not a list")
        sources = []
    for idx, src in enumerate(sources):
        if not isinstance(src, dict):
            plan.refuse(EXIT_CONFIG, "paper_sources[%d] is not an object" % idx)
            continue
        sid = str(src.get("id", "")).strip()
        v = src.get("db_file")
        if not isinstance(v, str) or not v.strip():
            plan.refuse(EXIT_CONFIG, "paper_sources[%s].db_file is empty" % sid)
            continue
        fields.append(("paper:" + sid, "paper_sources[%d].db_file" % idx, v, ["paper_sources", idx, "db_file"]))
    order = {"primary": 0, "paper": 1}
    fields.sort(key=lambda f: (order.get(f[0], 2), f[0]))
    target_names = {"primary": "state.db", "paper": "paper-state.db"}
    seen_canon = {}
    for role_name, label, value, keypath in fields:
        value = value.strip()
        if is_memory_db(value):
            plan.refuse(EXIT_CONFIG, "%s is an in-memory database (%s); there is no state file to move" % (label, value))
            continue
        lexical, canon = canonical_db(wd_real, value)
        if canon in seen_canon:
            plan.refuse(EXIT_CONFIG, "%s and %s resolve to one file %s" % (seen_canon[canon], label, canon))
            continue
        seen_canon[canon] = label
        norm_rel = os.path.normpath(value) if not os.path.isabs(value) else None
        rel_lex = rel_inside(lexical, wd_real)
        if norm_rel is not None and (norm_rel.startswith("scheduler/") and ".." not in norm_rel.split("/")):
            target = os.path.join(tgt_wd, norm_rel)
            new_value = None
        elif rel_lex is not None and rel_lex.startswith("scheduler/"):
            target = os.path.join(tgt_wd, rel_lex)
            new_value = target
        else:
            name = target_names.get(role_name) or ("paper-source-%s-state.db" % role_name.split(":", 1)[1])
            target = os.path.join(cfg_dir, name)
            new_value = target
        exists = os.path.exists(canon)
        if exists and not os.path.isfile(canon):
            plan.refuse(EXIT_CONFIG, "%s %s is not a regular file" % (label, canon))
        if os.path.exists(canon + "-journal"):
            plan.refuse(EXIT_CONFIG, "%s has a rollback journal %s-journal; the scheduler writes WAL only, so the file is in an unknown state" % (label, canon))
        owner = None
        mode = 0o644
        if exists:
            st = os.stat(canon)
            owner = [st.st_uid, st.st_gid]
            mode = stat.S_IMODE(st.st_mode)
        dbs.append({"role": role_name, "label": label, "value": value, "keypath": keypath, "source": canon,
                    "source_lexical": lexical, "target": target, "rewrite": new_value, "exists": exists,
                    "source_owner": owner, "mode": mode,
                    "source_dir_owner": [os.stat(os.path.dirname(canon)).st_uid, os.stat(os.path.dirname(canon)).st_gid] if os.path.isdir(os.path.dirname(canon)) else [0, 0]})
        if new_value is not None:
            rewrites.append({"keypath": keypath, "old": value, "new": new_value, "label": label})
    d["dbs"] = dbs
    tgts = [x["target"] for x in dbs]
    if len(set(tgts)) != len(tgts):
        plan.refuse(EXIT_CONFIG, "two databases map to one target path")
    for x in dbs:
        if main_pid and x["exists"]:
            state, holder = probe_lock(x["source"] + ".lock")
            if state != "held" or holder != main_pid:
                plan.refuse(EXIT_CONFIG, "discovery mismatch: %s maps to %s but the running pid %d does not hold %s.lock (%s %s)" % (x["label"], x["source"], main_pid, x["source"], state, holder or ""))
        elif main_pid and not x["exists"]:
            plan.note("%s %s does not exist yet; the new service creates it" % (x["label"], x["source"]))

    replay = cfg.get("replay_log_path")
    d["replay"] = None
    replay_set = set()
    if replay is not None and not isinstance(replay, str):
        plan.refuse(EXIT_CONFIG, "replay_log_path is not a string")
    elif isinstance(replay, str) and replay.strip():
        rp = replay.strip()
        if not os.path.isabs(rp):
            plan.refuse(EXIT_SHARED, "replay_log_path %s is relative, so it names another file after the move; set an absolute path under %s first" % (rp, SHARED_STATE_DIR))
        else:
            rreal = os.path.realpath(rp)
            if not within(rreal, SHARED_STATE_DIR) or rreal == SHARED_STATE_DIR:
                plan.refuse(EXIT_SHARED, "replay_log_path %s is outside %s, which is the only shared path the template makes writable" % (rp, SHARED_STATE_DIR))
            else:
                d["replay"] = rreal
                replay_set = {rreal, rreal + "-wal", rreal + "-shm"}
                plan.change("replay_log_path %s stays shared and unchanged; systemd gives %s to %s at the new unit's start" % (rp, SHARED_STATE_DIR, t_user))

    if main_pid:
        expected = set(replay_set)
        for x in dbs:
            for sfx in ("", "-wal", "-shm", ".lock", ".manual-action.lock"):
                expected.add(x["source"] + sfx)
        stray = []
        try:
            for fd in os.listdir("/proc/%d/fd" % main_pid):
                try:
                    target = os.readlink("/proc/%d/fd/%s" % (main_pid, fd))
                except OSError:
                    continue
                if not target.startswith("/"):
                    continue
                t = target.replace(" (deleted)", "")
                if re.search(r"(\.db|\.db-wal|\.db-shm|\.db-journal|\.lock|-wal|-shm|\.sqlite3?)$", t) and t not in expected:
                    stray.append(t)
        except OSError:
            plan.refuse(EXIT_CONFIG, "cannot read /proc/%d/fd to prove the database discovery" % main_pid)
        if stray:
            plan.refuse(EXIT_CONFIG, "the running daemon holds files discovery did not map: %s" % ", ".join(sorted(set(stray))))

    old_roots = sorted({wd, wd_real})
    known_paths = {tuple(r["keypath"]) for r in rewrites}

    def walk(obj, path):
        if isinstance(obj, dict):
            for k, v in obj.items():
                walk(v, path + [k])
        elif isinstance(obj, list):
            for i2, v in enumerate(obj):
                walk(v, path + [i2])
        elif isinstance(obj, str):
            if any(obj == r or obj.startswith(r.rstrip("/") + "/") or (r.rstrip("/") + "/") in obj for r in old_roots):
                if tuple(path) not in known_paths:
                    handled = False
                    if path == ["log_dir"] or (len(path) == 3 and path[0] == "strategies" and path[2] == "script"):
                        rel = None
                        for r in old_roots:
                            if within(obj, r):
                                rel = os.path.relpath(obj, r)
                        if rel is not None and not rel.startswith(".."):
                            rewrites.append({"keypath": path, "old": obj, "new": os.path.join(tgt_wd, rel), "label": ".".join(str(p) for p in path)})
                            handled = True
                    if not handled:
                        plan.refuse(EXIT_CONFIG, "config key %s names a path in the old tree; it cannot be moved safely" % ".".join(str(p) for p in path))

    walk(cfg, [])
    log_dir = cfg.get("log_dir") or "logs"
    if isinstance(log_dir, str):
        eff = None
        for r in rewrites:
            if r["keypath"] == ["log_dir"]:
                eff = r["new"]
        eff = eff or (log_dir if os.path.isabs(log_dir) else os.path.normpath(os.path.join(tgt_wd, log_dir)))
        writable = [os.path.join(tgt_wd, "scheduler"), os.path.join(tgt_wd, "logs"), cfg_dir, SHARED_STATE_DIR]
        if not any(within(eff, w) for w in writable):
            plan.refuse(EXIT_CONFIG, "log_dir resolves to %s, which the template does not make writable" % eff)
        d["log_dir"] = eff
    d["rewrites"] = rewrites
    for r in rewrites:
        plan.change("config %s: %s -> %s" % (r["label"], r["old"], r["new"]))
    new_cfg = json.loads(cfg_bytes.decode("utf-8"))
    for r in rewrites:
        ref = new_cfg
        for k in r["keypath"][:-1]:
            ref = ref[k]
        ref[r["keypath"][-1]] = r["new"]
    d["target_config_bytes"] = None if not rewrites else (json.dumps(new_cfg, indent=2, ensure_ascii=False) + "\n")

    env_pairs = parse_env_assignments(s.get("Environment", ""))
    if env_pairs is None:
        plan.refuse(EXIT_ENV, "cannot parse %s Environment=" % unit)
        env_pairs = []
    env_files = []
    for line in s.get("EnvironmentFiles", []) if isinstance(s.get("EnvironmentFiles"), list) else [s.get("EnvironmentFiles", "")]:
        line = line.strip()
        if not line:
            continue
        m = re.match(r"^(.*) \(ignore_errors=(yes|no)\)$", line)
        path, ign = (m.group(1), m.group(2)) if m else (line, "no")
        env_files.append((path, ign == "yes"))
    merged = {}
    for k, v in env_pairs:
        merged[k] = v
    d["env_files"] = []
    for path, ign in env_files:
        if not os.path.isfile(path):
            if ign:
                continue
            plan.refuse(EXIT_ENV, "EnvironmentFile %s is missing" % path)
            continue
        d["env_files"].append({"path": path, "fp": fingerprint(path)})
        for k, v in parse_env_file(path):
            merged[k] = v
    penv = proc_environ(main_pid) if main_pid else None
    if penv is None:
        plan.refuse(EXIT_ENV, "cannot read the running environment of pid %d" % main_pid)
    else:
        menv = manager_environment()
        for k in sorted(merged):
            if penv.get(k) != merged[k]:
                plan.refuse(EXIT_ENV, "the running process has a different %s than the unit's environment inputs; restart %s to apply them, then plan again" % (k, unit))
        for k in sorted(penv):
            if k in merged or k in SYSTEMD_ENV or k.startswith("LC_") or menv.get(k) == penv[k]:
                continue
            plan.refuse(EXIT_ENV, "the running process has %s from a source this tool cannot see" % k)
    target_env = {}
    for k in sorted(merged):
        v = merged[k]
        if k in FORBIDDEN_ENV:
            plan.refuse(EXIT_ENV, "%s is set in the unit's environment; the template forbids injecting it, so remove it first" % k)
            continue
        if "\n" in v or "\0" in v:
            plan.refuse(EXIT_ENV, "%s holds a multi-line value, which an environment file cannot carry safely" % k)
            continue
        if k == "GO_TRADER_SERVICE" and v.strip() in (unit, unit[:-len(".service")]):
            target_env[k] = target_unit
            plan.change("environment GO_TRADER_SERVICE: %s -> %s" % (v.strip(), target_unit))
            continue
        if any(r.rstrip("/") + "/" in v or v == r for r in old_roots):
            plan.refuse(EXIT_ENV, "environment %s names a path in the old tree" % k)
            continue
        if k == "GO_TRADER_SERVICE":
            plan.note("GO_TRADER_SERVICE names %s and is kept" % v.strip())
        target_env[k] = v
    d["target_env_keys"] = sorted(target_env)
    d["target_env_digest"] = hashlib.sha256(json.dumps(sorted(target_env.items())).encode("utf-8", "surrogateescape")).hexdigest()
    plan.change("environment: %d variable(s) from %d input(s) go to %s/.env (mode 0600); names: %s" % (len(target_env), len(env_pairs) + len(d["env_files"]), tgt_wd, ", ".join(sorted(target_env)) or "none"))
    plan._target_env = target_env

    venv_py = os.path.join(wd_real, ".venv", "bin", "python3")
    interp = os.path.realpath(venv_py)
    d["interpreter"] = interp
    venv_refs = []
    cfg_file = os.path.join(wd_real, ".venv", "pyvenv.cfg")
    if os.path.isfile(cfg_file):
        for line in open(cfg_file, errors="replace"):
            k, _, v = line.partition("=")
            if k.strip() == "home" and within(os.path.realpath(v.strip()), wd_real):
                venv_refs.append(cfg_file)
    for pth in sorted(glob.glob(os.path.join(wd_real, ".venv", "lib", "python*", "site-packages", "*.pth"))):
        text = open(pth, errors="replace").read()
        if any(r.rstrip("/") + "/" in text or text.strip() == r for r in (wd, wd_real)):
            venv_refs.append(pth)
    if within(interp, wd_real):
        plan.refuse(EXIT_RUNTIME, "the venv interpreter %s lives in the old tree, so the copied venv would still run it; rebuild the venv on an interpreter outside the tree first" % interp)
    elif venv_refs:
        plan.refuse(EXIT_RUNTIME, "the venv refers to the old tree in %s, so the copy would load code from there; rebuild the venv first" % ", ".join(venv_refs))
    else:
        ok = as_user_ok(t_user, [interp, "-c", "import encodings, sqlite3"]) if tuser else None
        if ok is None:
            ok = other_can(interp)
        if not ok:
            plan.refuse(EXIT_RUNTIME, "the venv interpreter %s is not usable by %s; install a Python the service account can read and rebuild the venv (uv sync) in the source first" % (interp, t_user))

    taken = []
    if os.path.lexists(tgt_wd):
        taken.append(tgt_wd + " exists")
    if os.path.lexists(cfg_dir):
        taken.append(cfg_dir + " exists")
    if os.path.lexists(os.path.join(UNIT_DIR, target_unit)):
        taken.append(os.path.join(UNIT_DIR, target_unit) + " exists")
    if os.path.lexists(os.path.join(UNIT_DIR, target_unit + ".d")):
        taken.append("drop-ins in " + os.path.join(UNIT_DIR, target_unit + ".d"))
    ts = systemctl_show(target_unit, ["LoadState", "ActiveState", "UnitFileState", "FragmentPath", "DropInPaths"])
    if ts.get("ActiveState") not in ("", "inactive"):
        taken.append("%s is %s" % (target_unit, ts.get("ActiveState")))
    if ts.get("UnitFileState") in ("enabled", "enabled-runtime", "masked", "masked-runtime", "linked", "linked-runtime"):
        taken.append("%s is %s" % (target_unit, ts.get("UnitFileState")))
    if (ts.get("DropInPaths") or "").strip():
        taken.append("%s has drop-ins" % target_unit)
    tfrag = ts.get("FragmentPath", "")
    if tfrag and tfrag != os.path.join(UNIT_DIR, TEMPLATE_NAME):
        taken.append("%s loads from %s" % (target_unit, tfrag))
    if instance in RESERVED_INSTANCES:
        taken.append("instance name %s is reserved" % instance)
    for other in discover_scheduler_units():
        if other == unit:
            continue
        ow = systemctl_show(other, ["WorkingDirectory"]).get("WorkingDirectory", "")
        if ow and within(os.path.realpath(ow), tgt_wd):
            taken.append("%s works in %s" % (other, ow))
    if taken:
        plan.refuse(EXIT_TAKEN, "the instance %s is taken: %s" % (instance, "; ".join(taken)))

    installed = os.path.join(UNIT_DIR, TEMPLATE_NAME)
    d["template_install"] = "install"
    if os.path.lexists(installed):
        if os.path.exists(installed) and fingerprint(installed) == d["template_fp"]:
            d["template_install"] = "same"
        else:
            plan.refuse(EXIT_SHARED, "%s differs from %s; other instances use it, so update them with scripts/update.sh --restart first" % (installed, tmpl_src))
    elif tfrag and tfrag != installed and ts.get("LoadState") == "loaded":
        plan.refuse(EXIT_SHARED, "%s loads from %s, not %s" % (target_unit, tfrag, installed))
    if d["template_install"] == "install":
        plan.change("installs %s (absent today)" % installed)
    d["journald_install"] = "none"
    if namespace:
        ver = out(["systemctl", "--version"], check=False).split()
        major = int(ver[1]) if len(ver) > 1 and ver[1].isdigit() else 0
        if major < 245:
            plan.change("systemd %s ignores LogNamespace=; logs stay in the default journal" % (major or "unknown"))
        else:
            src_conf = os.path.join(wd_real, "systemd", "journald@%s.conf" % namespace)
            dest = "/etc/systemd/journald@%s.conf" % namespace
            if not os.path.isfile(src_conf):
                plan.refuse(EXIT_SHARED, "%s is missing" % src_conf)
            elif os.path.islink(dest):
                plan.note("%s is a symlink and is left alone" % dest)
            elif os.path.exists(dest):
                if fingerprint(dest) != fingerprint(src_conf):
                    plan.refuse(EXIT_SHARED, "%s differs from %s; replacing it would change every go-trader unit's journal" % (dest, src_conf))
            else:
                d["journald_install"] = "install"
                plan.change("installs %s" % dest)
        plan.change("journal: logs move from 'journalctl -u %s' to 'journalctl --namespace=+%s -u %s'" % (unit, namespace, target_unit))

    if os.path.isdir(SHARED_STATE_DIR):
        foreign = []
        allowed_uids = {0}
        if tuser:
            allowed_uids.add(tuser["uid"])
        for root, dirs, files in os.walk(SHARED_STATE_DIR):
            for name in dirs + files:
                p = os.path.join(root, name)
                try:
                    if os.lstat(p).st_uid not in allowed_uids:
                        foreign.append(p)
                except OSError:
                    pass
        if foreign:
            plan.refuse(EXIT_SHARED, "%s holds files of another account (%s); the template's StateDirectory would give them to %s" % (SHARED_STATE_DIR, ", ".join(sorted(foreign)[:5]), t_user))
        elif tuser is None or os.stat(SHARED_STATE_DIR).st_uid != tuser["uid"]:
            plan.change("systemd gives %s to %s when the new unit starts; root services keep access" % (SHARED_STATE_DIR, t_user))

    if d["tree_mode"] == 0o700:
        plan.change("access: other accounts cannot reach %s, so %s is created 0700 %s:%s; files inside keep their modes" % (wd_real, tgt_wd, t_user, t_group))
    else:
        plan.change("access: other accounts can reach %s, so %s is 0755 %s:%s; files inside keep their modes" % (wd_real, tgt_wd, t_user, t_group))
    plan.change("access: config %s (%04o %s) -> %s (0600 %s:%s); %s is created 0700" % (
        cfg_real, d["config_mode"], owner_text(d["config_owner"]), d["target_config"], t_user, t_group, cfg_dir))
    for x in dbs:
        if x["exists"]:
            plan.change("access: database %s %s (%04o %s) -> %s (0600 %s:%s)" % (
                x["role"], x["source"], x["mode"], owner_text(x["source_owner"]), x["target"], t_user, t_group))

    peers = [u for u in discover_scheduler_units() if u != unit]
    if peers:
        plan.note("peer units left untouched: %s" % ", ".join(peers))
    plan.change("service references: scripts, cron jobs, monitoring and GO_TRADER_SERVICE users must name %s instead of %s" % (target_unit, unit))
    plan.change("tunnels: the status port stays %d, so SSH tunnels and proxies to 127.0.0.1:%d keep working" % (port, port))
    plan.change("the old unit %s is stopped, disabled and masked; its tree %s and config stay in place as evidence" % (unit, wd_real))

    cfg_dir_src = os.path.dirname(cfg_real)
    runtime_rel = set()
    rel_cfg_dir = rel_inside(cfg_dir_src, wd_real)
    if rel_cfg_dir is not None:
        for n in ("tuning_runs", "ohlcv_cache.sqlite3", "ohlcv_cache.sqlite3-wal", "ohlcv_cache.sqlite3-shm", "ohlcv_cache.sqlite3-journal"):
            runtime_rel.add(os.path.normpath(os.path.join(rel_cfg_dir, n)))
    d["runtime_excludes"] = sorted(runtime_rel)
    tuning_src = os.path.join(cfg_dir_src, "tuning_runs")
    d["tuning_runs_source"] = tuning_src if os.path.isdir(tuning_src) else None
    if d["tuning_runs_source"]:
        plan.change("tuning run history is copied from %s to %s/tuning_runs after the stop; the OHLCV cache starts empty" % (tuning_src, cfg_dir))
    plan.change("ownership: like the README template install, %s owns the new tree, its .env (0600) and %s; git and scripts/update.sh in the tree then run as that owner, or root marks it safe with git config --system --add safe.directory %s" % (t_user, cfg_dir, tgt_wd))
    au = cfg.get("auto_update") or "off"
    if au != "off":
        plan.change("auto_update=%s: the in-process upgrade cannot write the tree under the template sandbox; run scripts/update.sh --restart from a shell instead" % au)

    total = sum(os.path.getsize(x["source"]) + (os.path.getsize(x["source"] + "-wal") if os.path.exists(x["source"] + "-wal") else 0) for x in dbs if x["exists"])
    d["db_bytes"] = total
    tree_bytes = 0
    for root, dirs, files in os.walk(wd_real):
        if os.path.relpath(root, wd_real).split("/")[0] == "logs":
            continue
        for name in files:
            try:
                tree_bytes += os.lstat(os.path.join(root, name)).st_size
            except OSError:
                pass
    d["tree_bytes"] = tree_bytes

    stable = {
        "unit": unit, "instance": instance, "fragment_fp": d["fragment_fp"], "dropin_fps": d["dropin_fps"],
        "user": user, "group": group, "wd": wd_real, "config": cfg_real, "config_fp": d["config_fp"],
        "binary_sha": d["binary_sha"], "source_fp": d["source_fp"], "env_digest": d["target_env_digest"],
        "env_files": d["env_files"], "dbs": [(x["label"], x["source"], x["target"], x["rewrite"]) for x in dbs],
        "rewrites": [(r["label"], r["old"], r["new"]) for r in rewrites], "replay": d["replay"],
        "template_fp": d["template_fp"], "target_user": t_user, "port": port, "live": live, "tree_mode": d["tree_mode"],
        "version": d.get("version", ""), "unit_file_state": d["unit_file_state"],
    }
    d["plan_id"] = hashlib.sha256(json.dumps(stable, sort_keys=True).encode()).hexdigest()[:16]
    return plan


def print_plan(plan):
    d = plan.data
    if "source_dir" in d:
        log("source: unit=%s user=%s:%s dir=%s config=%s pid=%s version=%s port=%s live=%s" % (
            plan.unit, d.get("source_user"), d.get("source_group"), d.get("source_dir_real"), d.get("source_config"),
            d.get("main_pid"), d.get("version", "?"), d.get("status_port"), "yes" if d.get("live") else "no"))
        log("target: unit=%s user=%s:%s dir=%s config=%s" % (d["target_unit"], d.get("target_user"), d.get("target_group"), d["target_dir"], d["target_config"]))
    for x in d.get("dbs", []):
        log("database %s %s: %s -> %s%s%s" % (x["role"], x["label"], x["source"], x["target"],
                                        "" if x["exists"] else " (absent)",
                                        "" if not x["rewrite"] else " (config value becomes %s)" % x["rewrite"]))
    for c in plan.changes:
        log("change: " + c)
    for n in plan.notes:
        log("note: " + n)
    if d.get("live"):
        log("LIVE: apply needs --confirm-live %s" % plan.unit)
    if "exec_timeout" in d:
        log("execution proof waits up to %ds for every strategy to run (override with --exec-timeout)" % d["exec_timeout"])
    for code, msg in plan.refusals:
        log("refuse (exit %d): %s" % (code, msg))
    if "plan_id" in d and not plan.refusals:
        log("plan id: %s" % d["plan_id"])


def cmd_plan(args):
    plan = inspect(norm_unit(args.unit), args.instance, args.exec_timeout)
    print_plan(plan)
    if plan.refusals:
        sys.exit(plan.refusals[0][0])
    log("plan OK: nothing changed. Apply with: apply --unit %s --instance %s --plan-id %s%s" % (
        plan.unit, plan.instance, plan.data["plan_id"], (" --confirm-live %s" % plan.unit) if plan.data.get("live") else ""))


def inst_dir(instance):
    return os.path.join(STATE_ROOT, instance)


def journal_file(instance):
    return os.path.join(inst_dir(instance), "journal.jsonl")


def read_journal(instance):
    path = journal_file(instance)
    if not os.path.isfile(path):
        return []
    entries = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    entries.append(json.loads(line))
                except ValueError:
                    pass
    return entries


def current_txn(entries):
    txn = None
    for e in entries:
        if e.get("stage") == "begin":
            txn = e.get("txn")
    if txn is None:
        return None, []
    return txn, [e for e in entries if e.get("txn") == txn]


def txn_state(txn_entries):
    if not txn_entries:
        return "none"
    last = txn_entries[-1].get("stage")
    if last in TERMINAL_STATES:
        return last
    if last == "done":
        return "applied"
    return "incomplete"


def authority(txn_entries):
    for e in txn_entries:
        if e.get("stage") == "target-enable-intent":
            return "target"
    return "source"


def stage_entry(txn_entries, stage):
    found = None
    for e in txn_entries:
        if e.get("stage") == stage:
            found = e
    return found


class Txn:
    def __init__(self, instance, txn, manifest):
        self.instance = instance
        self.txn = txn
        self.m = manifest
        self.dir = os.path.join(inst_dir(instance), txn)
        self.evidence = os.path.join(self.dir, "evidence")
        self.source_locks = LockSet()
        self.target_locks = LockSet()

    def journal(self, stage, **fields):
        rec = {"txn": self.txn, "stage": stage, "at": now_iso()}
        rec.update(fields)
        path = journal_file(self.instance)
        with open(path, "a") as f:
            f.write(json.dumps(rec, sort_keys=True) + "\n")
            f.flush()
            os.fsync(f.fileno())
        fsync_dir(os.path.dirname(path))
        STAGE_SEEN[stage] = STAGE_SEEN.get(stage, 0) + 1
        here = (stage, "%s#%d" % (stage, STAGE_SEEN[stage]))
        if KILL_AFTER in here:
            os.kill(os.getpid(), signal.SIGKILL)
        if PAUSE_AT and PAUSE_AT.split(":", 1)[0] in here:
            sentinel = PAUSE_AT.split(":", 1)[1]
            log("test pause at %s until %s exists" % (stage, sentinel))
            while not os.path.exists(sentinel):
                time.sleep(0.2)
        if any(f in here for f in FAIL_AFTER.split(",") if f):
            raise StageFailure("MIGRATE_SERVICE_LAYOUT_FAIL_AFTER=%s" % FAIL_AFTER)

    def entries(self):
        return current_txn(read_journal(self.instance))[1]


def load_manifest(instance, txn):
    with open(os.path.join(inst_dir(instance), txn, "manifest.json")) as f:
        return json.load(f)


GLOBAL_LOCK_FD = None


def acquire_global_lock():
    global GLOBAL_LOCK_FD
    os.makedirs(STATE_ROOT, mode=0o700, exist_ok=True)
    os.chmod(STATE_ROOT, 0o700)
    fd = os.open(os.path.join(STATE_ROOT, "migrate.lock"), os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        os.close(fd)
        die(EXIT_BUSY, "another migrate-service-layout run holds %s/migrate.lock; wait for it or check 'status'. Nothing changed" % STATE_ROOT)
    GLOBAL_LOCK_FD = fd


INTERRUPT_LOG = {"instance": None}


def on_signal(signum, _frame):
    if signum == signal.SIGHUP and INTERRUPT_LOG["instance"]:
        try:
            p = os.path.join(inst_dir(INTERRUPT_LOG["instance"]), "interrupted.log")
            fd = os.open(p, os.O_CREAT | os.O_WRONLY | os.O_APPEND, 0o600)
            os.dup2(fd, 1)
            os.dup2(fd, 2)
            os.close(fd)
        except OSError:
            pass
    raise Interrupted("signal %d" % signum)


def install_signal_guards():
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGPIPE):
        signal.signal(sig, on_signal)


def ignore_signals():
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGPIPE):
        signal.signal(sig, signal.SIG_IGN)


def enablement_links(unit):
    links = []
    for root, dirs, files in os.walk(UNIT_DIR):
        for name in files + dirs:
            p = os.path.join(root, name)
            if name == unit and os.path.islink(p) and os.path.dirname(p) != UNIT_DIR:
                links.append({"path": p, "target": os.readlink(p)})
    return sorted(links, key=lambda x: x["path"])


def sandbox_run(t, argv, env_file=True, extra_rw=None, capture=True):
    m = t.m
    tgt = m["target_dir"]
    rw = [os.path.join(tgt, "scheduler"), os.path.join(tgt, "logs"), m["target_config_dir"]] + (extra_rw or [])
    rw = sorted(set(p for p in rw if os.path.isdir(p)))
    cmd = ["systemd-run", "--wait", "--pipe", "--collect", "--quiet",
           "--unit", "service-layout-check-%s" % secrets.token_hex(4),
           "--uid=%s" % m["target_user"], "--gid=%s" % m["target_group"],
           "-p", "WorkingDirectory=%s" % tgt, "-p", "ProtectSystem=strict", "-p", "PrivateTmp=true",
           "-p", "NoNewPrivileges=true", "-p", "ReadWritePaths=%s" % " ".join(rw),
           "-E", "PYTHONDONTWRITEBYTECODE=1"]
    if env_file:
        cmd += ["-p", "EnvironmentFile=%s" % os.path.join(tgt, ".env")]
    cmd += ["--"] + argv
    return run(cmd, check=False, timeout=600)


def chown_tree(path, uid, gid):
    os.lchown(path, uid, gid)
    for root, dirs, files in os.walk(path):
        for name in dirs + files:
            try:
                os.lchown(os.path.join(root, name), uid, gid)
            except OSError:
                pass


def build_excludes(m):
    ex = ["/scheduler/config.json", "/.env", "/logs/*", "/go-trader.pid", "/go-trader.new", "__pycache__/",
          "*.db", "*.db-wal", "*.db-shm", "*.db-journal", "*.db.lock", "*.manual-action.lock", "*.service-layout-*"]
    src = m["source_dir_real"]
    for x in m["dbs"]:
        for sfx in ("", "-wal", "-shm", "-journal", ".lock", ".manual-action.lock"):
            for base in (x["source"], x["source_lexical"]):
                rel = rel_inside(base + sfx, src)
                if rel:
                    ex.append("/" + rel)
    for peer_cfg in m.get("peer_configs_in_tree", []):
        ex.append("/" + peer_cfg)
    for rel in m.get("runtime_excludes", []):
        ex.append("/" + rel)
    return sorted(set(ex))


def stage_tree(t):
    m = t.m
    src, tgt = m["source_dir_real"], m["target_dir"]
    uid, gid = m["target_uid"], m["target_gid"]
    os.makedirs(tgt, mode=0o700)
    os.chmod(tgt, 0o700)
    t.journal("tree-intent", path=tgt)
    exfile = os.path.join(t.dir, "rsync.exclude")
    with open(exfile, "w") as f:
        f.write("\n".join(build_excludes(m)) + "\n")
    entries = [os.path.join(src, n) for n in sorted(os.listdir(src))]
    run(["rsync", "-a", "--numeric-ids", "--exclude-from=" + exfile] + entries + [tgt + "/"], timeout=3600)
    os.makedirs(os.path.join(tgt, "logs"), exist_ok=True)
    os.makedirs(os.path.join(tgt, "scheduler"), exist_ok=True)
    check = run(["rsync", "-a", "--numeric-ids", "--dry-run", "--checksum", "--itemize-changes",
                 "--exclude-from=" + exfile] + entries + [tgt + "/"], timeout=3600).stdout.decode("utf-8", "replace")
    diffs = [ln for ln in check.splitlines() if ln.strip() and not ln.startswith(".d") and not ln.startswith(".L")]
    if diffs:
        raise StageFailure("the copied tree differs from the source: %s" % "; ".join(diffs[:5]))
    if sha256_file(os.path.join(tgt, "go-trader")) != m["binary_sha"]:
        raise StageFailure("the copied binary differs from the source binary")
    if source_fingerprint(tgt) != m["source_fp"]:
        raise StageFailure("the copied tree's tracked source differs from the source")
    chown_tree(tgt, uid, gid)
    os.chmod(tgt, m["tree_mode"])
    t.journal("tree", path=tgt, binary_sha=m["binary_sha"], source_fp=m["source_fp"], mode="%04o" % m["tree_mode"])


def stage_config(t, target_env):
    m = t.m
    uid, gid = m["target_uid"], m["target_gid"]
    cdir = m["target_config_dir"]
    os.makedirs(cdir, mode=0o700)
    os.chmod(cdir, 0o700)
    t.journal("config-intent", path=cdir)
    os.chown(cdir, uid, gid)
    with open(m["source_config_real"], "rb") as f:
        raw = f.read()
    if hashlib.sha256(raw).hexdigest() != m["config_fp"]:
        raise StageFailure("the source config changed after the plan")
    write_atomic(os.path.join(t.evidence, "source-config.json"), raw, 0o600)
    data = raw if m["target_config_text"] is None else m["target_config_text"].encode("utf-8")
    write_atomic(m["target_config"], data, 0o600, uid, gid)
    src_obj = json.loads(raw.decode("utf-8"))
    tgt_obj = json.loads(open(m["target_config"], "rb").read().decode("utf-8"))
    for r in m["rewrites"]:
        ref = tgt_obj
        for k in r["keypath"][:-1]:
            ref = ref[k]
        if ref[r["keypath"][-1]] != r["new"]:
            raise StageFailure("config rewrite %s did not land" % r["label"])
        ref[r["keypath"][-1]] = r["old"]
    if tgt_obj != src_obj:
        raise StageFailure("the target config differs from the source beyond the recorded path changes")
    link = os.path.join(m["target_dir"], "scheduler", "config.json")
    if os.path.lexists(link):
        os.unlink(link)
    os.symlink(m["target_config"], link)
    os.lchown(link, uid, gid)
    env_path = os.path.join(m["target_dir"], ".env")
    body = "".join(env_file_line(k, target_env[k]) for k in sorted(target_env))
    write_atomic(env_path, body.encode("utf-8", "surrogateescape"), 0o600, uid, gid)
    parsed = dict(parse_env_file(env_path))
    if parsed != target_env:
        raise StageFailure("the written .env does not parse back to the same values")
    t.journal("config", path=m["target_config"], fp=fingerprint(m["target_config"]), env_keys=sorted(target_env))


def stage_verify_runtime(t, target_env):
    m = t.m
    tgt = m["target_dir"]
    probe_cfg = os.path.join(m["target_config_dir"], ".service-layout-probe.json")
    write_atomic(probe_cfg, open(m["target_config"], "rb").read(), 0o600, m["target_uid"], m["target_gid"])
    before = fingerprint(probe_cfg)
    p = sandbox_run(t, [os.path.join(tgt, "go-trader"), "probe", "--config", probe_cfg])
    after = fingerprint(probe_cfg)
    os.unlink(probe_cfg)
    text = (p.stdout + p.stderr).decode("utf-8", "replace")
    if p.returncode != 0 or "probe: OK" not in text:
        raise StageFailure("the binary's probe as %s under the template sandbox failed (exit %d): %s" % (m["target_user"], p.returncode, text.strip()[-800:]))
    if before != after:
        raise StageFailure("the probe rewrote its config copy (a config migration is pending); restart the source once, then retry")
    dirs = [os.path.join(tgt, "scheduler"), os.path.join(tgt, "logs"), m["target_config_dir"]]
    for x in m["dbs"]:
        dirs.append(os.path.dirname(x["target"]))
    script = "set -e\n" + "".join('touch "%s/.service-layout-write" && rm -f "%s/.service-layout-write"\n' % (dd, dd) for dd in sorted(set(dirs)))
    script += '"%s" -c "import sys, sqlite3; print(sys.prefix)"\n' % os.path.join(tgt, ".venv", "bin", "python3")
    p = sandbox_run(t, ["/bin/sh", "-c", script])
    if p.returncode != 0:
        raise StageFailure("write or Python check as %s failed: %s" % (m["target_user"], (p.stderr + p.stdout).decode("utf-8", "replace").strip()[-600:]))
    p = sandbox_run(t, ["/usr/bin/env", "-0"])
    if p.returncode != 0:
        raise StageFailure("environment dump under the template sandbox failed")
    got = {}
    for item in p.stdout.split(b"\0"):
        if b"=" in item:
            k, v = item.split(b"=", 1)
            got[k.decode("utf-8", "surrogateescape")] = v.decode("utf-8", "surrogateescape")
    bad = [k for k in sorted(target_env) if got.get(k) != target_env[k]]
    if bad:
        raise StageFailure("systemd reads %s/.env differently for: %s" % (tgt, ", ".join(bad)))
    t.journal("verify-runtime", probe="ok", env_keys=len(target_env))


def stage_templates(t):
    m = t.m
    installed_template = False
    installed_journald = False
    t.journal("template-intent")
    dest = os.path.join(UNIT_DIR, TEMPLATE_NAME)
    src = os.path.join(m["target_dir"], "systemd", TEMPLATE_NAME)
    if not os.path.lexists(dest):
        shutil.copyfile(src, dest)
        os.chmod(dest, 0o644)
        installed_template = True
    elif fingerprint(dest) != fingerprint(src):
        raise StageFailure("%s changed after the plan" % dest)
    ns = m["log_namespace"]
    if ns and m["journald_install"] == "install":
        jd = "/etc/systemd/journald@%s.conf" % ns
        if not os.path.lexists(jd):
            shutil.copyfile(os.path.join(m["target_dir"], "systemd", "journald@%s.conf" % ns), jd)
            os.chmod(jd, 0o644)
            installed_journald = True
            run(["systemctl", "try-restart", "systemd-journald@%s.service" % ns], check=False)
    daemon_reload()
    t.journal("template", installed_template=installed_template, installed_journald=installed_journald)


def wait_inactive(unit, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        st = unit_state(unit)
        if st["active"] in ("inactive", "failed") and st["pid"] == 0:
            return True
        time.sleep(1)
    return False


def stop_and_mask_source(t):
    m = t.m
    unit = m["unit"]
    t.journal("source-stop-intent", pid=m["main_pid"])
    p = run(["systemctl", "stop", unit], check=False, timeout=300)
    if p.returncode != 0 or not wait_inactive(unit):
        raise StageFailure("%s did not stop" % unit)
    t.journal("source-stopped")
    links = enablement_links(unit)
    frag = m["fragment"]
    ev_frag = os.path.join(t.evidence, "unit", os.path.basename(frag))
    os.makedirs(os.path.dirname(ev_frag), exist_ok=True)
    frag_is_etc = os.path.dirname(frag) == UNIT_DIR
    t.journal("source-mask-intent", links=links, fragment=frag, fragment_evidence=ev_frag if frag_is_etc else None,
              fragment_is_link=m["fragment_is_link"])
    for l in links:
        if os.path.islink(l["path"]):
            os.unlink(l["path"])
    mask_path = os.path.join(UNIT_DIR, unit)
    if frag_is_etc:
        if m["fragment_is_link"]:
            if not os.path.lexists(ev_frag):
                os.symlink(os.readlink(frag), ev_frag)
        elif not os.path.exists(ev_frag):
            copy_file_durable(frag, ev_frag)
        if not m["fragment_is_link"] and fingerprint(ev_frag) != m["fragment_fp"]:
            raise StageFailure("the saved copy of %s does not match it" % frag)
        tmp = mask_path + ".service-layout-mask"
        if os.path.lexists(tmp):
            os.unlink(tmp)
        os.symlink("/dev/null", tmp)
        os.replace(tmp, mask_path)
    else:
        if not os.path.lexists(mask_path):
            os.symlink("/dev/null", mask_path)
    fsync_dir(UNIT_DIR)
    daemon_reload()
    st = unit_state(unit)
    if st["load"] != "masked" or st["active"] not in ("inactive", "failed"):
        raise StageFailure("%s is not masked and inactive after the mask (load=%s active=%s)" % (unit, st["load"], st["active"]))
    t.journal("source-masked", links=links)


def restore_source_unit(t, start):
    m = t.m
    unit = m["unit"]
    txe = t.entries()
    mask_rec = stage_entry(txe, "source-mask-intent")
    mask_path = os.path.join(UNIT_DIR, unit)
    frag = m["fragment"]
    if mask_rec:
        frag_is_etc = os.path.dirname(frag) == UNIT_DIR
        if frag_is_etc:
            ev = mask_rec.get("fragment_evidence")
            cur_is_mask = os.path.islink(mask_path) and os.readlink(mask_path) == "/dev/null"
            if cur_is_mask or not os.path.lexists(mask_path):
                if m["fragment_is_link"]:
                    tmp = mask_path + ".service-layout-restore"
                    if os.path.lexists(tmp):
                        os.unlink(tmp)
                    os.symlink(os.readlink(ev), tmp)
                    os.replace(tmp, mask_path)
                else:
                    if not ev or fingerprint(ev) != m["fragment_fp"]:
                        raise StageFailure("the saved unit file %s is missing or changed" % ev)
                    tmp = mask_path + ".service-layout-restore"
                    shutil.copyfile(ev, tmp)
                    st = os.stat(ev)
                    os.chmod(tmp, stat.S_IMODE(st.st_mode))
                    os.chown(tmp, st.st_uid, st.st_gid)
                    os.replace(tmp, mask_path)
            elif fingerprint(mask_path) != m["fragment_fp"]:
                raise StageFailure("%s was changed outside the transaction" % mask_path)
        else:
            if os.path.islink(mask_path) and os.readlink(mask_path) == "/dev/null":
                os.unlink(mask_path)
        fsync_dir(UNIT_DIR)
    first_mask = next((e for e in txe if e.get("stage") == "source-mask-intent"), None)
    last_hold = None
    for e in txe:
        if e.get("stage") == "hold-source":
            last_hold = e
    links = (first_mask or {}).get("links", []) + (last_hold or {}).get("links", [])
    for l in links:
        if not os.path.lexists(l["path"]):
            os.makedirs(os.path.dirname(l["path"]), exist_ok=True)
            os.symlink(l["target"], l["path"])
    daemon_reload()
    st = unit_state(unit)
    if st["load"] != "loaded":
        raise StageFailure("%s did not load after the restore (LoadState=%s)" % (unit, st["load"]))
    if fingerprint(frag) != m["fragment_fp"] and not m["fragment_is_link"]:
        raise StageFailure("%s does not match its recorded fingerprint after the restore" % frag)
    if start and st["active"] != "active":
        run(["systemctl", "start", unit], check=False, timeout=300)
        pid = wait_healthy(unit, m["status_port"], m.get("version", ""), 180)
        if not pid:
            raise StageFailure("%s did not come back healthy on port %d" % (unit, m["status_port"]))
        return pid
    return unit_state(unit)["pid"]


def wait_healthy(unit, port, version, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        st = unit_state(unit)
        if st["active"] == "active" and st["pid"]:
            h = health(port)
            if h and h.get("pid") == st["pid"] and h.get("status") in ("ok",) and (not version or h.get("version") == version):
                return st["pid"]
        time.sleep(2)
    return 0


def uid_of_source(m):
    def f(db):
        o = db.get("source_owner") or db.get("source_dir_owner") or [0, 0]
        return o[0], o[1]
    return f


def uid_of_target(m):
    def f(_db):
        return m["target_uid"], m["target_gid"]
    return f


def stage_lock_and_snapshot(t):
    m = t.m
    acquire_db_locks(t.source_locks, m["dbs"], "source", uid_of_source(m))
    for x in m["dbs"]:
        os.makedirs(os.path.dirname(x["target"]), exist_ok=True)
        os.chown(os.path.dirname(x["target"]), m["target_uid"], m["target_gid"])
    acquire_db_locks(t.target_locks, m["dbs"], "target", uid_of_target(m))
    t.journal("locks", source=t.source_locks.paths(), target=t.target_locks.paths(), holder=os.getpid())
    snaps = []
    for x in m["dbs"]:
        canon = x["source"]
        before = db_fingerprint(canon)
        if before["journal"] != "none":
            raise StageFailure("%s has a rollback journal" % canon)
        rec = {"label": x["label"], "source": canon, "before": before, "db": None, "wal": None}
        if before["db"] != "absent":
            sdir = os.path.join(t.evidence, "snapshot")
            os.makedirs(sdir, exist_ok=True)
            name = re.sub(r"[^A-Za-z0-9_.-]", "_", x["label"])
            rec["db"] = os.path.join(sdir, name + ".db")
            copy_file_durable(canon, rec["db"], mode=0o600)
            if before["wal"] != "none":
                rec["wal"] = rec["db"] + "-wal"
                copy_file_durable(canon + "-wal", rec["wal"], mode=0o600)
            after = db_fingerprint(canon)
            if after != before:
                raise StageFailure("%s changed while it was copied" % canon)
            if fingerprint(rec["db"]) != before["db"] or (rec["wal"] and fingerprint(rec["wal"]) != before["wal"]):
                raise StageFailure("the snapshot of %s does not match the source bytes" % canon)
        snaps.append(rec)
    t.journal("snapshot", files=snaps)
    if m.get("tuning_runs_source") and os.path.isdir(m["tuning_runs_source"]):
        dest = os.path.join(m["target_config_dir"], "tuning_runs")
        run(["rsync", "-a", "--numeric-ids", m["tuning_runs_source"] + "/", dest + "/"], timeout=3600)
        chown_tree(dest, m["target_uid"], m["target_gid"])
        os.chmod(dest, 0o700)
        t.journal("tuning-runs", source=m["tuning_runs_source"], target=dest)
    return snaps


def stage_transfer(t, snaps):
    m = t.m
    results = []
    for x, rec in zip(m["dbs"], snaps):
        tgt = x["target"]
        if rec["db"] is None:
            for sfx in ("", "-wal", "-shm"):
                if os.path.lexists(tgt + sfx):
                    raise StageFailure("%s exists but the source file is absent" % (tgt + sfx))
            results.append({"label": x["label"], "target": tgt, "fp": "absent", "digest": None, "counts": {}})
            continue
        work = tempfile.mkdtemp(prefix=".service-layout-", dir=os.path.dirname(tgt))
        try:
            staged = os.path.join(work, "t.db")
            consolidate(rec["db"], rec["wal"], staged)
            src_digest = db_digest_pair(rec["db"], rec["wal"])
            tgt_digest = db_digest(staged)
            if src_digest["digest"] != tgt_digest["digest"]:
                raise StageFailure("the consolidated copy of %s differs in content" % x["label"])
            os.chmod(staged, 0o600)
            os.chown(staged, m["target_uid"], m["target_gid"])
            for sfx in ("-wal", "-shm"):
                if os.path.lexists(tgt + sfx):
                    os.unlink(tgt + sfx)
            os.replace(staged, tgt)
            fsync_dir(os.path.dirname(tgt))
        finally:
            shutil.rmtree(work, ignore_errors=True)
        results.append({"label": x["label"], "target": tgt, "fp": fingerprint(tgt), "digest": src_digest["digest"], "counts": src_digest["counts"]})
    t.journal("transfer", files=results)
    return results


def db_digest_pair(db, wal):
    work = tempfile.mkdtemp(prefix="service-layout-pair-")
    try:
        c = os.path.join(work, "p.db")
        shutil.copyfile(db, c)
        if wal:
            shutil.copyfile(wal, c + "-wal")
        return db_digest(c)
    finally:
        shutil.rmtree(work, ignore_errors=True)


def stage_validate(t, results):
    m = t.m
    for r in results:
        if r["digest"] is None:
            continue
        now = db_digest(r["target"])
        if now["digest"] != r["digest"]:
            raise StageFailure("%s content differs from the stopped source" % r["target"])
    cfg_copy = os.path.join(m["target_config_dir"], ".service-layout-inspect.json")
    write_atomic(cfg_copy, open(m["target_config"], "rb").read(), 0o600, m["target_uid"], m["target_gid"])
    before = fingerprint(cfg_copy)
    p = sandbox_run(t, [os.path.join(m["target_dir"], "go-trader"), "storage-inspect", "--json", "--config", cfg_copy],
                    extra_rw=[os.path.dirname(x["target"]) for x in m["dbs"]])
    after = fingerprint(cfg_copy)
    os.unlink(cfg_copy)
    if before != after:
        raise StageFailure("storage-inspect rewrote its config copy")
    try:
        rep = json.loads(p.stdout.decode("utf-8", "replace"))
    except ValueError:
        raise StageFailure("storage-inspect produced no JSON (exit %d): %s" % (p.returncode, p.stderr.decode("utf-8", "replace")[-600:]))
    if rep.get("rejections"):
        raise StageFailure("storage-inspect rejects the moved layout: %s" % "; ".join(rep["rejections"]))
    by_canon = {f.get("canonical_path"): f for f in rep.get("files") or []}
    summary = []
    for x, r in zip(m["dbs"], results):
        f = by_canon.get(x["target"])
        if f is None:
            raise StageFailure("storage-inspect does not list %s" % x["target"])
        if bool(f.get("present")) != (r["digest"] is not None):
            raise StageFailure("storage-inspect presence of %s differs" % x["target"])
        if not f.get("lock_held") or f.get("lock_holder_pid") != os.getpid():
            raise StageFailure("%s is not locked by this run during validation" % x["target"])
        c = r["counts"]
        summary.append("%s (%s): positions=%s trades=%s pending_manual_actions=%s portfolio_risk=%s partitions=%s" % (
            x["role"], x["label"], c.get("positions", 0), c.get("trades", 0), c.get("pending_manual_actions", 0),
            c.get("portfolio_risk", 0), ",".join(f.get("partitions_owned") or [])))
    for x in m["dbs"]:
        for sfx in ("", "-wal", "-shm"):
            if os.path.lexists(x["target"] + sfx):
                os.lchown(x["target"] + sfx, m["target_uid"], m["target_gid"])
    for line in summary:
        log("preserved " + line)
    t.journal("validated", summary=summary)


def assert_exclusive(t, starting):
    m = t.m
    src = unit_state(m["unit"])
    tgt = unit_state(m["target_unit"])
    if starting == "target":
        if src["active"] not in ("inactive", "failed") or src["pid"] or src["load"] != "masked":
            raise StageFailure("refusing to start %s: %s is %s/%s" % (m["target_unit"], m["unit"], src["load"], src["active"]))
        if not t.source_locks.held:
            raise StageFailure("refusing to start %s without holding the source locks" % m["target_unit"])
    else:
        if tgt["active"] not in ("inactive", "failed") or tgt["pid"]:
            raise StageFailure("refusing to start %s: %s is %s" % (m["unit"], m["target_unit"], tgt["active"]))
    log("exclusive: %s load=%s active=%s pid=%d; %s active=%s pid=%d; source locks held by pid %d: %s" % (
        m["unit"], src["load"], src["active"], src["pid"], m["target_unit"], tgt["active"], tgt["pid"], os.getpid(),
        "yes" if t.source_locks.held else "no"))


def stage_start_target(t):
    m = t.m
    t.target_locks.release()
    assert_exclusive(t, "target")
    t.journal("target-enable-intent")
    env = dict(os.environ)
    env["NO_START"] = "1"
    run(["bash", os.path.join(m["target_dir"], "scripts", "install-service.sh"),
         os.path.join(m["target_dir"], "systemd", TEMPLATE_NAME), m["instance"]], env=env, timeout=300)
    t.journal("target-installed")
    t.journal("target-start-attempt")
    started_at = time.time()
    run(["systemctl", "start", m["target_unit"]], check=False, timeout=300)
    pid = wait_healthy(m["target_unit"], m["status_port"], m.get("version", ""), 180)
    if not pid:
        h = health(m["status_port"])
        raise StageFailure("%s is not healthy on port %d with version %s (health: %s)" % (m["target_unit"], m["status_port"], m.get("version", ""), json.dumps(h)[:300] if h else "no answer"))
    st = systemctl_show(m["target_unit"], ["User", "ProtectSystem", "PrivateTmp", "NoNewPrivileges"])
    if st.get("User") != m["target_user"]:
        raise StageFailure("%s runs as %s, not %s" % (m["target_unit"], st.get("User"), m["target_user"]))
    t.journal("health", pid=pid, port=m["status_port"], version=m.get("version", ""), user=st.get("User"),
              protect_system=st.get("ProtectSystem"), private_tmp=st.get("PrivateTmp"))
    log("%s healthy: pid %d on port %d, version %s, user %s, ProtectSystem=%s PrivateTmp=%s" % (
        m["target_unit"], pid, m["status_port"], m.get("version", ""), st.get("User"), st.get("ProtectSystem"), st.get("PrivateTmp")))
    return pid, started_at


def stage_execution_proof(t, pid, started_at):
    m = t.m
    ids = m["strategy_ids"]
    deadline = time.time() + m["exec_timeout"]
    missing = ids
    last_save = None
    while time.time() < deadline:
        st = unit_state(m["target_unit"])
        if st["active"] != "active" or st["pid"] != pid:
            raise StageFailure("%s stopped or restarted during the execution proof" % m["target_unit"])
        h = health(m["status_port"]) or {}
        ev = h.get("run_evidence") or {}
        if h.get("pid") == pid and ev:
            evaluated = ev.get("evaluated") or {}
            skipped = ev.get("zero_capital_skipped") or {}
            held = ev.get("held")
            if not isinstance(held, dict):
                raise StageFailure("%s /health run_evidence has no held strategies; the binary predates the migration contract" % m["target_unit"])
            latched = {i for i, v in held.items() if isinstance(v, dict) and v.get("reason") == HELD_KILL_SWITCH}
            blocked = sorted("%s (%s)" % (i, v.get("reason") if isinstance(v, dict) else "unknown") for i, v in held.items() if i not in latched and i in ids)
            if blocked:
                raise StageFailure("%s held strategies for a reason other than the portfolio kill switch: %s" % (m["target_unit"], ", ".join(blocked)))
            missing = [i for i in ids if i not in evaluated and i not in skipped and i not in latched]
            last_save = parse_iso(ev.get("last_state_save"))
            latest = max([parse_iso(v) or 0 for v in evaluated.values()] or [0])
            if not missing and last_save and last_save >= latest and last_save >= started_at - 1:
                primary = m["dbs"][0]["target"]
                cycle = read_last_cycle(primary)
                if cycle is not None and cycle >= started_at - 1:
                    t.journal("execution", evaluated=sorted(evaluated), zero_capital_skipped=sorted(skipped),
                              kill_switch_held=sorted(latched), last_state_save=ev.get("last_state_save"))
                    held_only = sorted(i for i in ids if i in latched and i not in evaluated and i not in skipped)
                    log("execution proof: %d strategies ran, %d skipped at zero capital, %d held by the portfolio kill switch%s, state saved at %s and read back from %s" % (
                        len([i for i in ids if i in evaluated]), len([i for i in ids if i in skipped]), len(held_only),
                        " (%s)" % ", ".join(held_only) if held_only else "", ev.get("last_state_save"), primary))
                    return
        time.sleep(5)
    raise StageFailure("execution proof timed out after %ds: not yet evaluated: %s; last state save %s" % (
        m["exec_timeout"], ", ".join(missing) or "none", last_save))


def read_last_cycle(path):
    try:
        con = sqlite3.connect("file:%s?mode=ro" % path, uri=True, timeout=5)
        try:
            row = con.execute("SELECT last_cycle FROM app_state WHERE id = 1").fetchone()
        finally:
            con.close()
    except sqlite3.Error:
        return None
    return parse_iso(row[0]) if row and row[0] else None


def remove_target_artifacts(t, move_aside):
    m = t.m
    txe = t.entries()
    created = []
    if stage_entry(txe, "tree-intent"):
        created.append(m["target_dir"])
    if stage_entry(txe, "config-intent"):
        created.append(m["target_config_dir"])
    moved = []
    for p in created:
        aside_path = "%s.rolled-back-%s" % (p, t.txn)
        if not os.path.lexists(p):
            if move_aside and os.path.lexists(aside_path):
                moved.append(aside_path)
            continue
        if move_aside:
            dest = "%s.rolled-back-%s" % (p, t.txn)
            os.rename(p, dest)
            moved.append(dest)
        else:
            shutil.rmtree(p)
    tpl = stage_entry(txe, "template")
    if tpl and not move_aside:
        others = [u for u in discover_scheduler_units() if u.startswith("go-trader@") and u != m["target_unit"]]
        if tpl.get("installed_template") and not others:
            dest = os.path.join(UNIT_DIR, TEMPLATE_NAME)
            if os.path.exists(dest) and fingerprint(dest) == m["template_fp"]:
                os.unlink(dest)
        if tpl.get("installed_journald") and not others:
            jd = "/etc/systemd/journald@%s.conf" % m["log_namespace"]
            if os.path.exists(jd):
                os.unlink(jd)
    daemon_reload()
    return moved


def recover_source(t, why):
    ignore_signals()
    m = t.m
    warn("recovering the source: %s" % why)
    try:
        t.journal("recovery-begin", authority="source", reason=why)
        t.target_locks.release()
        tstate = unit_state(m["target_unit"])
        if tstate["active"] not in ("inactive", "failed") or tstate["file"] in ("enabled",):
            raise StageFailure("%s is %s/%s although it was never started" % (m["target_unit"], tstate["active"], tstate["file"]))
        snap = stage_entry(t.entries(), "snapshot")
        if snap:
            for rec in snap.get("files", []):
                now = db_fingerprint(rec["source"])
                if now != rec["before"]:
                    warn("%s changed while the source was stopped (%s -> %s); the source keeps its own current file" % (rec["source"], rec["before"], now))
        t.source_locks.release()
        was_active = m.get("source_active", True)
        pid = restore_source_unit(t, start=was_active)
        stopped = stage_entry(t.entries(), "source-stop-intent") is not None
        moved = remove_target_artifacts(t, move_aside=False)
        tmask = os.path.join(UNIT_DIR, m["target_unit"])
        if os.path.islink(tmask) and os.readlink(tmask) == "/dev/null":
            run(["systemctl", "unmask", m["target_unit"]], check=False)
            daemon_reload()
        t.journal("recovered-source", pid=pid, removed=[m["target_dir"], m["target_config_dir"]], moved=moved)
        log("%s runs again (pid %s) on its own files%s; the target was removed" % (m["unit"], pid, "" if stopped else " (it was never stopped)"))
        return True
    except Exception as e:
        hold_both(t, "source recovery failed: %s" % e)
        return False


def hold_both(t, why):
    m = t.m
    try:
        run(["systemctl", "stop", m["target_unit"]], check=False, timeout=300)
        run(["systemctl", "disable", m["target_unit"]], check=False)
        if not os.path.lexists(os.path.join(UNIT_DIR, m["target_unit"])):
            run(["systemctl", "mask", m["target_unit"]], check=False)
    except Exception:
        pass
    try:
        src = unit_state(m["unit"])
        if src["load"] != "masked":
            links = enablement_links(m["unit"])
            try:
                t.journal("hold-source", links=links)
            except Exception as e:
                warn("could not journal the enable links of %s (%s); they were: %s" % (m["unit"], e, json.dumps(links)))
            run(["systemctl", "stop", m["unit"]], check=False, timeout=300)
            run(["systemctl", "disable", m["unit"]], check=False)
    except Exception:
        pass
    try:
        t.journal("recovery-failed", reason=why, evidence=t.evidence)
    except Exception:
        pass
    warn("RECOVERY FAILED: %s" % why)
    warn("both %s and %s are held stopped; evidence in %s; journal %s" % (m["unit"], m["target_unit"], t.evidence, journal_file(t.instance)))


def reverse_transfer(t, why, explicit):
    ignore_signals()
    m = t.m
    unit, tunit = m["unit"], m["target_unit"]
    txe = t.entries()
    snap = stage_entry(txe, "snapshot")
    if snap is None:
        raise StageFailure("the journal has no snapshot record")
    done = stage_entry(txe, "recovery-transferred")
    if explicit and done is None:
        problems = source_conflicts(t, snap)
        _data, cfg_err = config_back_mapping(t)
        if cfg_err:
            problems.append(cfg_err)
        if problems:
            die(EXIT_ROLLBACK_REFUSED, "the source changed outside the transaction (%s); this run changed nothing, and %s is %s" % (
                "; ".join(problems), tunit, unit_state(tunit)["active"]))
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    try:
        t.journal("recovery-begin", authority="target", reason=why, resume=done is not None)
        run(["systemctl", "stop", tunit], check=False, timeout=300)
        run(["systemctl", "disable", tunit], check=False)
        if not os.path.lexists(os.path.join(UNIT_DIR, tunit)):
            run(["systemctl", "mask", "--runtime", tunit], check=False)
        if not wait_inactive(tunit):
            raise StageFailure("%s did not stop" % tunit)
        t.journal("target-stopped")
        if done is None:
            rdir = transfer_back(t, snap, stamp)
        else:
            rdir = done.get("dir") or ""
            log("the target's records were already returned in an earlier attempt; finishing the rollback")
        t.target_locks.release()
        assert_exclusive(t, "source")
        t.source_locks.release()
        pid = restore_source_unit(t, start=m.get("source_active", True))
        aside = remove_target_artifacts(t, move_aside=True)
        run(["systemctl", "unmask", "--runtime", tunit], check=False)
        run(["systemctl", "unmask", tunit], check=False)
        daemon_reload()
        t.journal("rolled-back", pid=pid, target_moved_to=aside, evidence=rdir)
        log("%s runs again (pid %s) with the target's latest records; target files moved to %s; evidence in %s" % (unit, pid, ", ".join(aside) or "nowhere", rdir))
        return True
    except SystemExit:
        raise
    except Exception as e:
        hold_both(t, "reverse transfer failed: %s" % e)
        return False


def transfer_back(t, snap, stamp):
    m = t.m
    t.source_locks.release()
    t.target_locks.release()
    acquire_db_locks(t.source_locks, m["dbs"], "source", uid_of_source(m))
    acquire_db_locks(t.target_locks, m["dbs"], "target", uid_of_target(m))
    t.journal("recovery-locks", holder=os.getpid())
    problems = source_conflicts(t, snap)
    if problems:
        raise StageFailure("the source changed outside the transaction: %s" % "; ".join(problems))
    cfg_data, cfg_err = config_back_mapping(t)
    if cfg_err:
        raise StageFailure(cfg_err)
    rdir = os.path.join(t.evidence, "recovery-" + stamp)
    os.makedirs(rdir, exist_ok=True)
    plan = []
    for x in m["dbs"]:
        tgt = x["target"]
        before = db_fingerprint(tgt)
        rec = {"label": x["label"], "target": tgt, "source": x["source"], "before": before, "db": None, "wal": None}
        name = re.sub(r"[^A-Za-z0-9_.-]", "_", x["label"])
        if before["db"] != "absent":
            rec["db"] = os.path.join(rdir, name + ".target.db")
            copy_file_durable(tgt, rec["db"], mode=0o600)
            if before["wal"] != "none":
                rec["wal"] = rec["db"] + "-wal"
                copy_file_durable(tgt + "-wal", rec["wal"], mode=0o600)
            if db_fingerprint(tgt) != before:
                raise StageFailure("%s changed while it was copied" % tgt)
        for sfx in ("", "-wal"):
            if os.path.exists(x["source"] + sfx):
                copy_file_durable(x["source"] + sfx, os.path.join(rdir, name + ".source-before" + (".db" if not sfx else ".db-wal")), mode=0o600)
        plan.append(rec)
    t.journal("recovery-snapshot", files=plan, dir=rdir)
    moved = []
    for x, rec in zip(m["dbs"], plan):
        src = x["source"]
        if rec["db"] is None:
            continue
        os.makedirs(os.path.dirname(src), exist_ok=True)
        work = tempfile.mkdtemp(prefix=".service-layout-", dir=os.path.dirname(src))
        try:
            staged = os.path.join(work, "s.db")
            consolidate(rec["db"], rec["wal"], staged)
            want = db_digest_pair(rec["db"], rec["wal"])
            if db_digest(staged)["digest"] != want["digest"]:
                raise StageFailure("the consolidated recovery copy of %s differs in content" % x["label"])
            owner = x.get("source_owner") or x.get("source_dir_owner") or [0, 0]
            os.chmod(staged, x["mode"])
            os.chown(staged, owner[0], owner[1])
            t.journal("recovery-write-intent", file=src, fp=fingerprint(staged))
            for sfx in ("-wal", "-shm"):
                if os.path.lexists(src + sfx):
                    os.unlink(src + sfx)
            os.replace(staged, src)
            fsync_dir(os.path.dirname(src))
        finally:
            shutil.rmtree(work, ignore_errors=True)
        if db_digest(src)["digest"] != want["digest"]:
            raise StageFailure("%s does not hold the target's records after the transfer" % src)
        moved.append({"label": x["label"], "file": src, "counts": want["counts"], "fp": fingerprint(src)})
    if cfg_data is not None:
        shutil.copyfile(m["source_config_real"], os.path.join(rdir, "source-config.before.json"))
        shutil.copyfile(m["target_config"], os.path.join(rdir, "target-config.json"))
        st = os.stat(m["source_config_real"])
        t.journal("recovery-config-intent", fp=hashlib.sha256(cfg_data).hexdigest())
        write_atomic(m["source_config_real"], cfg_data, stat.S_IMODE(st.st_mode), st.st_uid, st.st_gid)
        log("the target changed its config during its run; the change is carried back to %s" % m["source_config_real"])
    t.journal("recovery-transferred", files=moved, dir=rdir)
    for mv in moved:
        c = mv["counts"]
        log("returned %s to %s: positions=%s trades=%s pending_manual_actions=%s portfolio_risk=%s" % (
            mv["label"], mv["file"], c.get("positions", 0), c.get("trades", 0), c.get("pending_manual_actions", 0), c.get("portfolio_risk", 0)))
    return rdir


def source_conflicts(t, snap):
    m = t.m
    problems = []
    written = {}
    config_written = set()
    for e in t.entries():
        if e.get("stage") == "recovery-write-intent":
            written.setdefault(e.get("file"), set()).add(e.get("fp"))
        if e.get("stage") == "recovery-config-intent":
            config_written.add(e.get("fp"))
    for rec in snap.get("files", []):
        now = db_fingerprint(rec["source"])
        if now == rec["before"]:
            continue
        mine = written.get(rec["source"], set())
        if mine and now["wal"] == "none" and now["journal"] == "none" and (now["db"] in mine or now["db"] == rec["before"]["db"]):
            continue
        problems.append("%s is %s, recorded %s" % (rec["source"], now, rec["before"]))
    cfp = fingerprint(m["source_config_real"])
    if cfp != m["config_fp"] and cfp not in config_written:
        problems.append("config %s changed" % m["source_config_real"])
    return problems


def config_back_mapping(t):
    m = t.m
    cfg_entry = stage_entry(t.entries(), "config")
    if not cfg_entry or not os.path.exists(m["target_config"]):
        return None, None
    if fingerprint(m["target_config"]) == cfg_entry.get("fp"):
        return None, None
    raw = open(m["target_config"], "rb").read()
    try:
        obj = json.loads(raw.decode("utf-8"))
    except ValueError as e:
        return None, "the target config does not parse (%s)" % e
    for r in m["rewrites"]:
        ref = obj
        try:
            for k in r["keypath"][:-1]:
                ref = ref[k]
            cur = ref[r["keypath"][-1]]
        except (KeyError, IndexError, TypeError):
            cur = None
        if cur != r["new"]:
            return None, "the target config changed %s, so it cannot be mapped back" % r["label"]
        ref[r["keypath"][-1]] = r["old"]
    data = raw if not m["rewrites"] else (json.dumps(obj, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
    return data, None


def cmd_apply(args):
    unit = norm_unit(args.unit)
    instance = args.instance
    acquire_global_lock()
    INTERRUPT_LOG["instance"] = instance
    txn_id, txe = current_txn(read_journal(instance))
    state = txn_state(txe)
    if state == "applied":
        man = load_manifest(instance, txn_id)
        if man["unit"] == unit:
            log("already applied in transaction %s: %s runs as %s; nothing changed" % (txn_id, unit, man["target_unit"]))
            return
        die(EXIT_TAKEN, "instance %s already holds %s from transaction %s" % (instance, man["unit"], txn_id))
    if state == "incomplete":
        die(EXIT_BUSY, "transaction %s for instance %s is incomplete (last stage %s); run 'rollback --instance %s' first" % (txn_id, instance, txe[-1].get("stage"), instance))
    plan = inspect(unit, instance, args.exec_timeout)
    print_plan(plan)
    if plan.refusals:
        sys.exit(plan.refusals[0][0])
    d = plan.data
    if d.get("live") and args.confirm_live not in (unit, unit[:-len(".service")]):
        die(EXIT_CONFIRM, "%s trades live or manual strategies; pass --confirm-live %s. Nothing changed" % (unit, unit))
    if args.plan_id and args.plan_id != d["plan_id"]:
        die(EXIT_STALE, "the inspected inputs changed since plan %s (now %s); review 'plan' again. Nothing changed" % (args.plan_id, d["plan_id"]))
    txn = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(3)
    free_state = shutil.disk_usage(STATE_ROOT).free
    if d["db_bytes"] * 3 + (64 << 20) > free_state:
        die(EXIT_RUNTIME, "%s needs about %d bytes free for snapshots and recovery copies; it has %d. Nothing changed" % (STATE_ROOT, d["db_bytes"] * 3, free_state))
    free_target = shutil.disk_usage("/opt").free
    if d["tree_bytes"] + d["db_bytes"] * 2 + (64 << 20) > free_target:
        die(EXIT_RUNTIME, "/opt needs about %d bytes for the tree copy and the state files; it has %d. Nothing changed" % (d["tree_bytes"] + d["db_bytes"] * 2, free_target))
    tdir = os.path.join(inst_dir(instance), txn)
    os.makedirs(os.path.join(tdir, "evidence"), mode=0o700)
    os.chmod(inst_dir(instance), 0o700)
    tuser = user_info(d["target_user"])
    m = dict(d)
    m.pop("show", None)
    m["txn"] = txn
    m["target_config_text"] = d["target_config_bytes"]
    m.pop("target_config_bytes", None)
    m["source_active"] = True
    peers_in_tree = []
    for other in discover_scheduler_units():
        if other == unit:
            continue
        ocfg = config_path_for(other)
        rel = rel_inside(ocfg, d["source_dir_real"]) if ocfg else None
        if rel:
            peers_in_tree.append(rel)
    m["peer_configs_in_tree"] = sorted(set(peers_in_tree))
    t = Txn(instance, txn, m)
    install_signal_guards()
    try:
        m["target_uid"] = tuser["uid"] if tuser else None
        m["target_gid"] = tuser["gid"] if tuser else None
        write_atomic(os.path.join(tdir, "manifest.json"), json.dumps(m, indent=2, sort_keys=True).encode(), 0o600)
        t.journal("begin", unit=unit, target_unit=m["target_unit"], plan_id=m["plan_id"], live=m["live"])
        if tuser is None:
            t.journal("account-intent", user=m["target_user"])
            create_account(m["target_user"], m["target_group"])
            tuser = user_info(m["target_user"])
            t.journal("account", created=True, uid=tuser["uid"])
        m["target_uid"] = tuser["uid"]
        m["target_gid"] = tuser["gid"]
        write_atomic(os.path.join(tdir, "manifest.json"), json.dumps(m, indent=2, sort_keys=True).encode(), 0o600)
        stage_tree(t)
        stage_config(t, plan._target_env)
        stage_verify_runtime(t, plan._target_env)
        stage_templates(t)
        recheck = inspect_recheck(unit, instance, args.exec_timeout, m)
        if recheck:
            raise StageFailure("the inputs changed during the preparation: %s" % recheck)
        t.journal("prepared")
    except Exception as e:
        ignore_signals()
        warn("apply failed before %s stopped: %s" % (unit, e))
        try:
            remove_target_artifacts(t, move_aside=False)
            t.journal("aborted", reason=str(e))
        except Exception as e2:
            warn("could not remove the target artifacts: %s" % e2)
        die(EXIT_PRESTOP_FAILED, "nothing changed on %s; the target artifacts were removed" % unit)
    try:
        stop_and_mask_source(t)
        snaps = stage_lock_and_snapshot(t)
        results = stage_transfer(t, snaps)
        stage_validate(t, results)
    except Exception as e:
        if recover_source(t, str(e)):
            die(EXIT_SOURCE_RESTORED, "apply failed before %s started (%s); %s runs again on its own files" % (m["target_unit"], e, unit))
        die(EXIT_RECOVERY_FAILED, "apply failed before %s started (%s) and the source recovery failed" % (m["target_unit"], e))
    try:
        pid, started_at = stage_start_target(t)
        stage_execution_proof(t, pid, started_at)
        t.journal("done", pid=pid)
    except Exception as e:
        warn("apply failed after %s was started: %s" % (m["target_unit"], e))
        if reverse_transfer(t, str(e), explicit=False):
            die(EXIT_TARGET_RETURNED, "apply failed after %s started (%s); its records were returned and %s runs again" % (m["target_unit"], e, unit))
        die(EXIT_RECOVERY_FAILED, "apply failed after %s started (%s) and the recovery failed" % (m["target_unit"], e))
    ignore_signals()
    t.source_locks.release()
    log("apply OK: %s runs as %s under %s; %s is stopped, disabled and masked. Roll back with: rollback --instance %s%s" % (
        m["target_unit"], m["target_user"], m["target_dir"], unit, instance, (" --confirm-live %s" % unit) if m["live"] else ""))


def inspect_recheck(unit, instance, exec_timeout, m):
    s = systemctl_show(unit, ["ActiveState", "MainPID", "NeedDaemonReload"])
    problems = []
    if s.get("ActiveState") != "active" or int(s.get("MainPID", "0") or "0") != m["main_pid"]:
        problems.append("%s restarted or stopped" % unit)
    if s.get("NeedDaemonReload") == "yes":
        problems.append("%s unit files changed" % unit)
    if fingerprint(m["fragment"]) != m["fragment_fp"]:
        problems.append("unit file changed")
    for p, fp in m["dropin_fps"].items():
        if fingerprint(p) != fp:
            problems.append("drop-in %s changed" % p)
    if fingerprint(m["source_config_real"]) != m["config_fp"]:
        problems.append("config changed")
    for ef in m["env_files"]:
        if fingerprint(ef["path"]) != ef["fp"]:
            problems.append("environment file %s changed" % ef["path"])
    if sha256_file(m["binary"]) != m["binary_sha"]:
        problems.append("binary changed")
    return "; ".join(problems)


def create_account(user, group):
    nologin = shutil.which("nologin") or "/usr/sbin/nologin"
    if not os.path.exists(nologin):
        nologin = "/bin/false"
    try:
        grp.getgrnam(group)
        run(["useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", nologin, "-g", group, user])
    except KeyError:
        if group == user:
            run(["useradd", "--system", "--user-group", "--no-create-home", "--home-dir", "/nonexistent", "--shell", nologin, user])
        else:
            run(["groupadd", "--system", group])
            run(["useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", nologin, "-g", group, user])


def cmd_rollback(args):
    instance = args.instance
    acquire_global_lock()
    INTERRUPT_LOG["instance"] = instance
    txn_id, txe = current_txn(read_journal(instance))
    if txn_id is None:
        die(EXIT_ROLLBACK_REFUSED, "no migration transaction is recorded for instance %s" % instance)
    state = txn_state(txe)
    if state in TERMINAL_STATES:
        log("transaction %s is already closed (%s); nothing changed" % (txn_id, state))
        return
    m = load_manifest(instance, txn_id)
    unit = m["unit"]
    if m.get("live") and args.confirm_live not in (unit, unit[:-len(".service")]):
        die(EXIT_CONFIRM, "%s trades live or manual strategies; pass --confirm-live %s. Nothing changed" % (unit, unit))
    t = Txn(instance, txn_id, m)
    if m.get("target_uid") is None:
        tu = user_info(m["target_user"])
        if tu:
            m["target_uid"], m["target_gid"] = tu["uid"], tu["gid"]
    if authority(txe) == "source":
        log("transaction %s stopped at %s before the target could start; restoring %s" % (txn_id, txe[-1].get("stage"), unit))
        if recover_source(t, "rollback of an incomplete transaction"):
            log("rollback OK")
            return
        die(EXIT_RECOVERY_FAILED, "the source recovery failed")
    if stage_entry(txe, "snapshot") is None:
        die(EXIT_RECOVERY_FAILED, "the journal has target authority but no snapshot record")
    if reverse_transfer(t, "explicit rollback" if state == "applied" else "rollback of an interrupted transaction", explicit=True):
        log("rollback OK")
        return
    die(EXIT_RECOVERY_FAILED, "the rollback failed")


def cmd_status(args):
    if not os.path.isdir(STATE_ROOT):
        log("no migration has run on this host (%s is absent)" % STATE_ROOT)
        return
    instances = [args.instance] if args.instance else sorted(n for n in os.listdir(STATE_ROOT) if os.path.isdir(os.path.join(STATE_ROOT, n)))
    for inst in instances:
        entries = read_journal(inst)
        txn, txe = current_txn(entries)
        if txn is None:
            log("%s: no transaction" % inst)
            continue
        state = txn_state(txe)
        try:
            m = load_manifest(inst, txn)
        except (OSError, ValueError):
            m = {}
        log("%s: transaction %s %s (last stage %s at %s; authority %s)" % (inst, txn, state, txe[-1].get("stage"), txe[-1].get("at"), authority(txe)))
        for unit in (m.get("unit"), m.get("target_unit")):
            if unit:
                st = unit_state(unit)
                log("  %s: load=%s active=%s file=%s pid=%d" % (unit, st["load"], st["active"], st["file"], st["pid"]))
        if m.get("status_port"):
            h = health(m["status_port"])
            log("  port %d /health: %s" % (m["status_port"], "pid=%s version=%s status=%s" % (h.get("pid"), h.get("version"), h.get("status")) if h else "no answer"))
        for x in m.get("dbs", []):
            for side in ("source", "target"):
                lk = probe_lock(x[side] + ".lock") if os.path.exists(x[side] + ".lock") else ("absent", 0)
                log("  %s %s %s lock=%s%s" % (x["label"], side, x[side], lk[0], (" pid=%d" % lk[1]) if lk[1] else ""))
        if args.instance:
            for e in txe:
                log("  journal: %s" % json.dumps(e, sort_keys=True))
        if state == "incomplete" or stage_entry(txe, "recovery-failed"):
            log("  ACTION: run 'rollback --instance %s'%s" % (inst, (" --confirm-live %s" % m.get("unit")) if m.get("live") else ""))


def main():
    ap = argparse.ArgumentParser(prog="migrate-service-layout.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd")
    p = sub.add_parser("plan")
    p.add_argument("--unit", required=True)
    p.add_argument("--instance", required=True)
    p.add_argument("--exec-timeout", type=int)
    p = sub.add_parser("apply")
    p.add_argument("--unit", required=True)
    p.add_argument("--instance", required=True)
    p.add_argument("--confirm-live", default="")
    p.add_argument("--plan-id", default="")
    p.add_argument("--exec-timeout", type=int)
    p = sub.add_parser("rollback")
    p.add_argument("--instance", required=True)
    p.add_argument("--confirm-live", default="")
    p = sub.add_parser("status")
    p.add_argument("--instance")
    args = ap.parse_args()
    if not args.cmd:
        ap.print_help()
        sys.exit(EXIT_USAGE)
    if os.geteuid() != 0:
        die(EXIT_USAGE, "must run as root")
    if getattr(args, "instance", None) is not None and not valid_instance(args.instance):
        die(EXIT_USAGE, "invalid instance name %r (letters, digits, dot, dash, underscore; no leading dash)" % args.instance)
    if getattr(args, "exec_timeout", None) is not None and args.exec_timeout <= 0:
        die(EXIT_USAGE, "--exec-timeout must be positive")
    {"plan": cmd_plan, "apply": cmd_apply, "rollback": cmd_rollback, "status": cmd_status}[args.cmd](args)


if __name__ == "__main__":
    main()
