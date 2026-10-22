import hashlib
import os
import stat
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, ".."))
if _BACKTEST not in sys.path:
    sys.path.insert(0, _BACKTEST)

import archive_cache as ac  # noqa: E402

STUB = """#!{python}
import os, shutil, sys
args = sys.argv[1:]
remote = os.environ["STUB_REMOTE"]
with open(os.environ["STUB_LOG"], "a") as fh:
    fh.write(" ".join(args) + "\\n")
assert args[-2:] == ["--request-payer", "requester"]
if args[:2] == ["s3", "cp"]:
    key = args[2].split("/", 3)[3]
    src = os.path.join(remote, key)
    if not os.path.exists(src):
        sys.stderr.write("fatal error: An error occurred (404) when calling the HeadObject operation: Not Found\\n")
        sys.exit(1)
    shutil.copyfile(src, args[3])
    sys.exit(0)
if args[:2] == ["s3api", "head-object"]:
    key = args[args.index("--key") + 1]
    if os.path.exists(os.path.join(remote, key)):
        sys.exit(0)
    sys.stderr.write("An error occurred (404) when calling the HeadObject operation: Not Found\\n")
    sys.exit(254)
sys.exit(2)
"""


@pytest.fixture
def archive(tmp_path, monkeypatch):
    remote = tmp_path / "remote"
    bindir = tmp_path / "bin"
    bindir.mkdir()
    stub = bindir / "aws"
    stub.write_text(STUB.format(python=sys.executable))
    stub.chmod(0o755)
    log = tmp_path / "aws.log"
    log.write_text("")
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ['PATH']}")
    monkeypatch.setenv("STUB_REMOTE", str(remote))
    monkeypatch.setenv("STUB_LOG", str(log))
    objects = {"market_data/20260801/0/l2Book/BTC.lz4": b"book bytes", "asset_ctxs/20260801.csv.lz4": b"ctx bytes"}
    for key, blob in objects.items():
        (remote / key).parent.mkdir(parents=True, exist_ok=True)
        (remote / key).write_bytes(blob)
    files = [{"key": k, "status": "present", "size": len(b), "sha256": hashlib.sha256(b).hexdigest()}
             for k, b in sorted(objects.items())]
    files.append({"key": "market_data/20260801/1/l2Book/BTC.lz4", "status": "absent"})
    manifest = {"bucket": "stub-bucket", "files": files}
    root = tmp_path / "cache"
    base = ac.bucket_dir(str(root), "stub-bucket")
    return {"remote": remote, "root": str(root), "base": base, "manifest": manifest, "log": log}


def _downloads(log) -> int:
    return sum(1 for line in log.read_text().splitlines() if line.startswith("s3 cp"))


def test_fetch_downloads_once_and_writes_read_only_verified_files(archive):
    first = ac.fetch_manifest(archive["root"], archive["manifest"])
    assert (first["downloaded"], first["verified"], first["absent"]) == (2, 0, 1)
    for f in archive["manifest"]["files"]:
        if f["status"] == "present":
            path = ac.verified_object(archive["base"], f["key"], f)
            assert not os.stat(path).st_mode & (stat.S_IWUSR | stat.S_IWGRP | stat.S_IWOTH)
    assert not [n for _, _, names in os.walk(archive["root"]) for n in names if n.endswith(".part")]
    second = ac.fetch_manifest(archive["root"], archive["manifest"])
    assert (second["downloaded"], second["verified"], second["absent"]) == (0, 2, 1)
    assert _downloads(archive["log"]) == 2


def test_altered_or_missing_cached_files_refuse_and_are_never_replaced(archive):
    ac.fetch_manifest(archive["root"], archive["manifest"])
    book = archive["manifest"]["files"][1]
    ctx = archive["manifest"]["files"][0]
    path = os.path.join(archive["base"], book["key"])
    os.chmod(path, 0o644)
    with open(path, "ab") as fh:
        fh.write(b"x")
    with pytest.raises(ac.ArchiveError, match=f"{book['key']}: the cached file .* does not match the manifest"):
        ac.fetch_manifest(archive["root"], archive["manifest"])
    with pytest.raises(ac.ArchiveError, match="does not match the manifest"):
        ac.verified_object(archive["base"], book["key"], book)
    assert open(path, "rb").read() == b"book bytesx"
    os.chmod(os.path.join(archive["base"], ctx["key"]), 0o644)
    os.unlink(os.path.join(archive["base"], ctx["key"]))
    with pytest.raises(ac.ArchiveError, match=f"{ctx['key']}: the manifest lists it as present but .* is missing"):
        ac.verified_object(archive["base"], ctx["key"], ctx)


def test_downloaded_bytes_that_differ_from_the_manifest_never_reach_the_final_path(archive):
    ctx = archive["manifest"]["files"][0]
    (archive["remote"] / ctx["key"]).write_bytes(b"other bytes")
    with pytest.raises(ac.ArchiveError, match=f"{ctx['key']}: the downloaded object does not match the manifest"):
        ac.fetch_manifest(archive["root"], archive["manifest"], workers=1)
    assert not os.path.lexists(os.path.join(archive["base"], ctx["key"]))
    assert not [n for _, _, names in os.walk(archive["root"]) for n in names if n.endswith(".part")]


def test_an_absent_key_that_the_archive_now_holds_refuses(archive):
    absent = archive["manifest"]["files"][2]
    (archive["remote"] / absent["key"]).parent.mkdir(parents=True, exist_ok=True)
    (archive["remote"] / absent["key"]).write_bytes(b"late")
    with pytest.raises(ac.ArchiveError, match=f"{absent['key']}: the manifest lists it as absent but the archive now holds it"):
        ac.fetch_manifest(archive["root"], archive["manifest"])


def test_freeze_pins_present_and_absent_keys_and_refuses_cache_conflicts(archive):
    keys = [f["key"] for f in archive["manifest"]["files"]]
    assert ac.freeze(archive["root"], "stub-bucket", keys) == archive["manifest"]["files"]
    ac.fetch_manifest(archive["root"], archive["manifest"])
    assert _downloads(archive["log"]) == 3
    (archive["remote"] / keys[0]).write_bytes(b"changed")
    with pytest.raises(ac.ArchiveError, match=f"{keys[0]}: the cached file .* does not match"):
        ac.freeze(archive["root"], "stub-bucket", keys)


def test_cache_root_resolution(monkeypatch, tmp_path):
    monkeypatch.setenv(ac.ARCHIVE_CACHE_DIR_ENV, "   ")
    with pytest.raises(ac.ArchiveError, match=f"{ac.ARCHIVE_CACHE_DIR_ENV} must not be blank"):
        ac.cache_root()
    monkeypatch.setenv(ac.ARCHIVE_CACHE_DIR_ENV, str(tmp_path / "explicit"))
    assert ac.cache_root() == str(tmp_path / "explicit")
    monkeypatch.delenv(ac.ARCHIVE_CACHE_DIR_ENV)
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "xdg"))
    assert ac.cache_root() == str(tmp_path / "xdg" / "go-trader" / "archive")
    for bad in ("../escape", "/abs/key", "a//b", "a/./b"):
        with pytest.raises(ac.ArchiveError, match="not a plain relative name"):
            ac.object_path(str(tmp_path), bad)
