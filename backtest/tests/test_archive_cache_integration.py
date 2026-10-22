import errno
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
if os.environ.get("STUB_STDERR"):
    sys.stderr.write(os.environ["STUB_STDERR"].replace("@DEST@", args[3] if args[:2] == ["s3", "cp"] else "") + "\\n")
    sys.exit(int(os.environ.get("STUB_RC", "1")))
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


KEY_404 = "market_data/20240404/0/l2Book/BTC.lz4"


def _calls(log, prefix: str) -> int:
    return sum(1 for line in log.read_text().splitlines() if line.startswith(prefix))


def test_a_connection_error_naming_a_404_key_retries_and_never_records_absent(archive, monkeypatch):
    monkeypatch.setattr(ac.time, "sleep", lambda _: None)
    monkeypatch.setenv("STUB_RC", "255")
    monkeypatch.setenv("STUB_STDERR", f'Could not connect to the endpoint URL: "https://hyperliquid-archive.s3.amazonaws.com/{KEY_404}"')
    with pytest.raises(ac.ArchiveError, match=f"{KEY_404}: download failed: Could not connect"):
        ac.freeze(archive["root"], "stub-bucket", [KEY_404])
    assert _calls(archive["log"], "s3 cp") == ac.AWS_ATTEMPTS
    manifest = {"bucket": "stub-bucket", "files": [{"key": KEY_404, "status": "absent"}]}
    with pytest.raises(ac.ArchiveError, match=f"{KEY_404}: head-object failed: Could not connect"):
        ac.fetch_manifest(archive["root"], manifest)
    assert _calls(archive["log"], "s3api head-object") == ac.AWS_ATTEMPTS


def test_a_real_not_found_error_is_absent_without_retries(archive, monkeypatch):
    monkeypatch.setattr(ac.time, "sleep", lambda _: pytest.fail("a not-found error was retried"))
    monkeypatch.setenv("STUB_RC", "254")
    monkeypatch.setenv("STUB_STDERR", "aws: [ERROR]: An error occurred (404) when calling the HeadObject operation: Not Found")
    assert ac.freeze(archive["root"], "stub-bucket", [KEY_404]) == [{"key": KEY_404, "status": "absent"}]
    assert (_calls(archive["log"], "s3 cp"), _calls(archive["log"], "s3api head-object")) == (1, 1)


def test_a_service_error_through_a_404_cache_root_is_retried_and_not_missing(archive, monkeypatch, tmp_path):
    monkeypatch.setattr(ac.time, "sleep", lambda _: None)
    root = str(tmp_path / "u404" / "cache")
    monkeypatch.setenv("STUB_STDERR", "download failed: s3://stub-bucket/" + KEY_404 + " to @DEST@ An error occurred (503) when calling the GetObject operation: Slow Down")
    entry = {"key": KEY_404, "status": "present", "size": 1, "sha256": "0" * 64}
    with pytest.raises(ac.ArchiveError, match=f"{KEY_404}: download failed: .*u404.*\\(503\\)"):
        ac.fetch_manifest(root, {"bucket": "stub-bucket", "files": [entry]})
    assert _calls(archive["log"], "s3 cp") == ac.AWS_ATTEMPTS
    assert not [n for _, _, names in os.walk(root) for n in names]


def _parts(root: str) -> list:
    return [n for _, _, names in os.walk(root) for n in names if n.endswith(".part")]


def test_a_missing_aws_executable_leaves_no_temporary_file(archive, monkeypatch, tmp_path):
    empty = tmp_path / "empty-bin"
    empty.mkdir()
    monkeypatch.setenv("PATH", str(empty))
    with pytest.raises(FileNotFoundError):
        ac.fetch_manifest(archive["root"], archive["manifest"])
    assert os.path.isdir(archive["base"]) and _parts(archive["root"]) == []


def test_an_aws_callable_that_raises_leaves_no_temporary_file(archive):
    def boom(args):
        raise RuntimeError("aws crashed")
    keys = [f["key"] for f in archive["manifest"]["files"]]
    with pytest.raises(RuntimeError, match="aws crashed"):
        ac.freeze(archive["root"], "stub-bucket", keys, aws=boom, workers=1)
    assert os.path.isdir(archive["base"]) and _parts(archive["root"]) == []


def test_without_hard_links_a_final_path_placed_during_download_is_never_replaced(archive, monkeypatch):
    def no_link(src, dst):
        raise OSError(errno.EPERM, "hard links unsupported")
    monkeypatch.setattr(ac.os, "link", no_link)
    ctx, book = archive["manifest"]["files"][0], archive["manifest"]["files"][1]
    final = os.path.join(archive["base"], ctx["key"])

    def racing(args):
        r = ac.run_aws(args)
        if args[:2] == ["s3", "cp"] and ctx["key"] in args[2]:
            with open(final, "wb") as fh:
                fh.write(b"placed by another worktree")
        return r
    with pytest.raises(ac.ArchiveError, match=f"{ctx['key']}: the cached file .* does not match the manifest"):
        ac.fetch_manifest(archive["root"], {"bucket": "stub-bucket", "files": [ctx]}, aws=racing, workers=1)
    assert open(final, "rb").read() == b"placed by another worktree"
    assert _parts(archive["root"]) == []
    out = ac.fetch_manifest(archive["root"], {"bucket": "stub-bucket", "files": [book]}, workers=1)
    assert out["downloaded"] == 1
    assert ac.verified_object(archive["base"], book["key"], book)
