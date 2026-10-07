import fnmatch
import io
import posixpath
import sys
import tarfile

FORBIDDEN_NAMES = {".env", "go-trader.env", "config.json", "state.json", "trading_bot.db"}
FORBIDDEN_GLOBS = [
    "*.db", "*.db-wal", "*.db-shm", "*.db-journal", "*.db.lock",
    "*.sqlite", "*.sqlite3", "*.sqlite3-wal", "*.sqlite3-shm",
    "*.pid", "*.log", "*.bak", "*.prev",
]
EMPTY_DIRS = ("data", "app/logs")


def app_owned(path):
    return path.startswith("app/") and not path.startswith("app/.venv/")


def check_path(path, problems):
    while path.startswith("./"):
        path = path[2:]
    path = path.lstrip("/")
    base = posixpath.basename(path)
    if base.startswith(".wh."):
        return
    parts = path.split("/")
    if ".git" in parts:
        problems.append(f"git metadata: {path}")
    if "tuning_runs" in parts and app_owned(path):
        problems.append(f"tuning artifacts: {path}")
    for d in EMPTY_DIRS:
        if path.startswith(d + "/") and path.rstrip("/") != d:
            problems.append(f"content in a mount target: {path}")
    if app_owned(path) or path.startswith("data/"):
        if base in FORBIDDEN_NAMES:
            problems.append(f"runtime file: {path}")
    if app_owned(path) or path.startswith("data/") or path.startswith("app/.venv/"):
        for g in FORBIDDEN_GLOBS:
            if fnmatch.fnmatch(base, g):
                problems.append(f"state, lock or log file: {path}")
                break


def scan_layer(name, data, canary, problems, counts):
    try:
        layer = tarfile.open(fileobj=io.BytesIO(data), mode="r:*")
    except tarfile.TarError:
        return False
    with layer:
        for m in layer:
            counts["entries"] += 1
            check_path(m.name, problems)
            if canary and m.isfile():
                f = layer.extractfile(m)
                if f is not None and canary in f.read():
                    problems.append(f"canary bytes in {m.name} (layer {name})")
    return True


def main():
    archive = sys.argv[1]
    canary = sys.argv[2].encode() if len(sys.argv) > 2 and sys.argv[2] else b""
    problems = []
    counts = {"layers": 0, "entries": 0}
    with tarfile.open(archive) as outer:
        for m in outer:
            if not m.isfile():
                continue
            data = outer.extractfile(m).read()
            if m.name.endswith((".json", "repositories", "oci-layout")):
                continue
            if scan_layer(m.name, data, canary, problems, counts):
                counts["layers"] += 1
    if counts["layers"] == 0:
        print("no image layers found in the saved archive", file=sys.stderr)
        return 1
    for p in sorted(set(problems)):
        print(p, file=sys.stderr)
    print(f"scanned {counts['layers']} layers, {counts['entries']} entries, {len(set(problems))} problems")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
