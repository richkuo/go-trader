import hashlib
import os
import re
import subprocess
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Callable, Dict, List

ARCHIVE_CACHE_DIR_ENV = "GO_TRADER_ARCHIVE_CACHE_DIR"
FETCH_WORKERS = 8
AWS_ATTEMPTS = 6
_NOT_FOUND = re.compile(r"An error occurred \((?:404|NoSuchKey)\)")


class ArchiveError(ValueError):
    pass


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def cache_root() -> str:
    configured = os.environ.get(ARCHIVE_CACHE_DIR_ENV)
    if configured is None:
        base = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
        return os.path.abspath(os.path.join(base, "go-trader", "archive"))
    configured = configured.strip()
    if not configured:
        raise ArchiveError(f"{ARCHIVE_CACHE_DIR_ENV} must not be blank")
    return os.path.abspath(os.path.expanduser(configured))


def _plain_parts(name: str, what: str) -> List[str]:
    parts = name.split("/")
    if not name or any(p in ("", ".", "..") for p in parts):
        raise ArchiveError(f"{what} {name!r} is not a plain relative name")
    return parts


def bucket_dir(root: str, bucket: str) -> str:
    parts = _plain_parts(bucket, "bucket")
    if len(parts) != 1:
        raise ArchiveError(f"bucket {bucket!r} is not a plain relative name")
    return os.path.join(root, bucket)


def object_path(base: str, key: str) -> str:
    return os.path.join(base, *_plain_parts(key, "key"))


def run_aws(args: list) -> subprocess.CompletedProcess:
    return subprocess.run(["aws"] + args + ["--request-payer", "requester"], capture_output=True, text=True)


def _not_found(stderr: str) -> bool:
    return _NOT_FOUND.search(stderr) is not None


def _call(aws: Callable, args: list) -> subprocess.CompletedProcess:
    for attempt in range(AWS_ATTEMPTS):
        r = aws(args)
        if r.returncode == 0 or _not_found(r.stderr) or attempt == AWS_ATTEMPTS - 1:
            return r
        time.sleep(2 * (attempt + 1))


def _mismatch(path: str, entry: dict) -> str:
    size = os.path.getsize(path)
    digest = sha256_file(path)
    if size == entry["size"] and digest == entry["sha256"]:
        return ""
    return f"file size {size} sha256 {digest}, manifest size {entry['size']} sha256 {entry['sha256']}"


def verified_object(base: str, key: str, entry: dict) -> str:
    path = object_path(base, key)
    if not os.path.isfile(path):
        raise ArchiveError(f"{key}: the manifest lists it as present but {path} is missing (run fetch)")
    bad = _mismatch(path, entry)
    if bad:
        raise ArchiveError(f"{key}: the cached file {path} does not match the manifest size and sha256 ({bad})")
    return path


def _download(bucket: str, key: str, path: str, aws: Callable) -> str:
    parent = os.path.dirname(path)
    os.makedirs(parent, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=parent, prefix="." + os.path.basename(path) + ".", suffix=".part")
    os.close(fd)
    try:
        r = _call(aws, ["s3", "cp", f"s3://{bucket}/{key}", tmp, "--only-show-errors"])
    except BaseException:
        os.unlink(tmp)
        raise
    if r.returncode != 0:
        os.unlink(tmp)
        if _not_found(r.stderr):
            return ""
        raise ArchiveError(f"{key}: download failed: {r.stderr.strip()}")
    return tmp


def _publish(tmp: str, path: str) -> bool:
    os.chmod(tmp, 0o444)
    try:
        os.link(tmp, path)
    except FileExistsError:
        return False
    except OSError:
        if os.path.lexists(path):
            return False
        os.replace(tmp, path)
        return True
    os.unlink(tmp)
    return True


def _place(base: str, bucket: str, entry: dict, aws: Callable) -> str:
    key = entry["key"]
    path = object_path(base, key)
    if os.path.lexists(path):
        verified_object(base, key, entry)
        return "verified"
    tmp = _download(bucket, key, path, aws)
    if not tmp:
        raise ArchiveError(f"{key}: the manifest lists it as present but the archive does not hold it")
    try:
        bad = _mismatch(tmp, entry)
        if bad:
            raise ArchiveError(f"{key}: the downloaded object does not match the manifest size and sha256 ({bad})")
        if _publish(tmp, path):
            return "downloaded"
        verified_object(base, key, entry)
        return "verified"
    finally:
        if os.path.lexists(tmp):
            os.unlink(tmp)


def _confirm_absent(bucket: str, key: str, aws: Callable) -> str:
    r = _call(aws, ["s3api", "head-object", "--bucket", bucket, "--key", key])
    if r.returncode == 0:
        raise ArchiveError(f"{key}: the manifest lists it as absent but the archive now holds it")
    if not _not_found(r.stderr):
        raise ArchiveError(f"{key}: head-object failed: {r.stderr.strip()}")
    return "absent"


def _run_all(fn: Callable, items: list, workers: int) -> list:
    pool = ThreadPoolExecutor(max_workers=workers)
    futures = [pool.submit(fn, item) for item in items]
    try:
        out = [f.result() for f in futures]
    except BaseException:
        pool.shutdown(wait=True, cancel_futures=True)
        raise
    pool.shutdown(wait=True)
    return out


def fetch_manifest(root: str, manifest: dict, aws: Callable = run_aws, workers: int = FETCH_WORKERS) -> Dict[str, object]:
    bucket = manifest["bucket"]
    base = bucket_dir(root, bucket)
    keys = [f["key"] for f in manifest["files"]]
    if len(set(keys)) != len(keys):
        raise ArchiveError("the manifest lists a key more than once")
    for f in manifest["files"]:
        object_path(base, f["key"])
        if f["status"] not in ("present", "absent"):
            raise ArchiveError(f"{f['key']}: unknown manifest status {f['status']!r}")
        if f["status"] == "present" and not (isinstance(f.get("size"), int) and isinstance(f.get("sha256"), str) and len(f["sha256"]) == 64):
            raise ArchiveError(f"{f['key']}: a present manifest entry needs an integer size and a sha256")

    def one(entry: dict) -> str:
        if entry["status"] == "present":
            return _place(base, bucket, entry, aws)
        return _confirm_absent(bucket, entry["key"], aws)
    outcomes = _run_all(one, manifest["files"], workers)
    return {"cache": base, "downloaded": outcomes.count("downloaded"), "verified": outcomes.count("verified"),
            "absent": outcomes.count("absent")}


def freeze(root: str, bucket: str, keys: list, aws: Callable = run_aws, workers: int = FETCH_WORKERS) -> List[dict]:
    base = bucket_dir(root, bucket)
    if len(set(keys)) != len(keys):
        raise ArchiveError("the key list names a key more than once")
    for key in keys:
        object_path(base, key)

    def one(key: str) -> dict:
        path = object_path(base, key)
        tmp = _download(bucket, key, path, aws)
        if not tmp:
            _confirm_absent(bucket, key, aws)
            if os.path.lexists(path):
                raise ArchiveError(f"{key}: the archive does not hold it but the cache has {path}")
            return {"key": key, "status": "absent"}
        try:
            entry = {"key": key, "status": "present", "size": os.path.getsize(tmp), "sha256": sha256_file(tmp)}
            if os.path.lexists(path) or not _publish(tmp, path):
                verified_object(base, key, entry)
            return entry
        finally:
            if os.path.lexists(tmp):
                os.unlink(tmp)
    return _run_all(one, keys, workers)
