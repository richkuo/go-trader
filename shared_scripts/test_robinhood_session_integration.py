import hashlib
import json
import os
import pickle
import stat
import subprocess
import sys
import time
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent
SCRIPTS = REPO_ROOT / "shared_scripts"
FIXTURE = SCRIPTS / "testdata" / "robinhood_offline"
SESSION_ENV = "GO_TRADER_ROBINHOOD_SESSION_DIR"
USERNAME = "session-user"
PASSWORD = "session-password"
TOTP = "JBSWY3DPEHPK3PXP"
TOKEN_URL = "https://api.robinhood.com/oauth2/token/"
POSITIONS_CHECK = "https://api.robinhood.com/positions/?nonzero=true"
HOLDINGS = "https://nummus.robinhood.com/holdings/"
PATHFINDER = "https://api.robinhood.com/pathfinder/"
MACHINE = PATHFINDER + "user_machine/"
INQUIRY = PATHFINDER + "inquiries/machine-1/user_view/"
CHALLENGE = "https://api.robinhood.com/challenge/challenge-1/respond/"
LOGIN_FAILED = "Robinhood login failed:"
BOOTSTRAP = "go-trader robinhood-login --config"
STUB_ACCESS = "offline-access-token"
STUB_REFRESH = "offline-refresh-token"
STORED_ACCESS = "stored-access-token"
STORED_REFRESH = "stored-refresh-token"
STORED_DEVICE = "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
SECRETS = (USERNAME, PASSWORD, TOTP, STUB_ACCESS, STUB_REFRESH, STORED_ACCESS, STORED_REFRESH)
TOKEN_OK = {"status": 200, "json": {
    "access_token": STUB_ACCESS,
    "token_type": "Bearer",
    "refresh_token": STUB_REFRESH,
    "expires_in": 86400,
    "scope": "internal",
}}
TOKEN_REFUSED = {"status": 400, "json": {"detail": "Unable to log in with provided credentials."}}
VERIFICATION = {"status": 200, "json": {"verification_workflow": {"id": "workflow-1"}}}
EMPTY_PAGE = {"status": 200, "json": {"next": None, "results": []}}
FETCH = ["fetch_robinhood_positions.py"]
SIX_SCRIPTS = [
    pytest.param(["check_robinhood.py", "momentum_pro", "BTC", "1h", "--mode=live"], None, id="check_live"),
    pytest.param(["close_robinhood_position.py", "--symbol=BTC", "--mode=live", "--quantity=0.5"], None, id="close"),
    pytest.param(FETCH, None, id="positions"),
    pytest.param(["check_options.py", "vol_mean_reversion", "BTC", "--platform=robinhood"], "[]", id="options"),
    pytest.param(["check_balance.py", "--platform=robinhood"], None, id="balance"),
    pytest.param(["fetch_candles.py", "--platform", "robinhood", "--type", "spot", "--symbol", "BTC",
                  "--timeframe", "1h", "--limit", "50", "--mode", "live"], None, id="candles_live"),
]


def _key(username):
    return hashlib.sha256(("go-trader/robinhood-username/v1:" + username.strip().lower()).encode()).hexdigest()[:16]


class Box:

    def __init__(self, tmp_path):
        self.root = Path(os.path.realpath(tmp_path))
        self.folder = self.root / "robinhood-session"
        self.home = self.root / "home"
        self.routes = self.root / "routes.json"
        self.log = self.root / "requests.jsonl"
        self.set_routes({})

    def set_routes(self, routes):
        self.routes.write_text(json.dumps(routes))

    def file(self, suffix, username=USERNAME):
        return self.folder / f"robinhood-{_key(username)}.{suffix}"

    def env(self, username=USERNAME, folder=None, extra=None):
        env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE", SESSION_ENV))}
        env.update({
            "HOME": str(self.home),
            "PYTHONPATH": str(FIXTURE),
            "PYTHONDONTWRITEBYTECODE": "1",
            "RH_OFFLINE_STUB_MODE": "routes",
            "RH_OFFLINE_STUB_ROUTES": str(self.routes),
            "RH_OFFLINE_STUB_LOG": str(self.log),
            "ROBINHOOD_USERNAME": username,
            "ROBINHOOD_PASSWORD": PASSWORD,
            "ROBINHOOD_TOTP_SECRET": TOTP,
        })
        if folder is not False:
            env[SESSION_ENV] = str(self.folder) if folder is None else folder
        env.update(extra or {})
        return env

    def requests(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines() if line.strip()]

    def reset_log(self):
        if self.log.exists():
            self.log.unlink()

    def seed_session(self, device=STORED_DEVICE, mode=0o600):
        self.folder.mkdir(mode=0o700, exist_ok=True)
        path = self.file("pickle")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as f:
            pickle.dump({
                "token_type": "Bearer",
                "access_token": STORED_ACCESS,
                "refresh_token": STORED_REFRESH,
                "device_token": device,
            }, f)
        os.chmod(path, mode)
        return path.read_bytes()


def _run(box, argv, stdin_text=None, umask=None, **env_kwargs):
    kwargs = {"input": stdin_text} if stdin_text is not None else {"stdin": subprocess.DEVNULL}
    if umask is not None:
        kwargs["preexec_fn"] = lambda: os.umask(umask)
    started = time.monotonic()
    proc = subprocess.run(
        [sys.executable, str(SCRIPTS / argv[0]), *argv[1:]],
        cwd=str(REPO_ROOT),
        env=box.env(**env_kwargs),
        capture_output=True,
        text=True,
        timeout=120,
        **kwargs,
    )
    proc.elapsed = time.monotonic() - started
    return proc


def _doc(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, proc.stdout
    return json.loads(lines[0])


def _detail(proc, box):
    return f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{box.requests()}"


def _no_secrets(proc):
    return [s for s in SECRETS if s in proc.stdout or s in proc.stderr]


def _token_posts(made):
    return [r for r in made if r["method"] == "POST" and r["url"] == TOKEN_URL]


def _count(made, url, method="GET"):
    return len([r for r in made if r["method"] == method and r["url"] == url])


def _mode(path):
    return stat.S_IMODE(os.lstat(path).st_mode)


def _home_pickles(home):
    if not home.exists():
        return []
    return [p for p in home.rglob("robinhood*.pickle")]


@pytest.mark.parametrize("argv,stdin_text", SIX_SCRIPTS)
def test_every_script_stores_the_session_only_in_the_configured_folder(tmp_path, argv, stdin_text):
    box = Box(tmp_path)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE]})
    proc = _run(box, argv, stdin_text=stdin_text)
    detail = _detail(proc, box)
    assert box.file("pickle").is_file(), detail
    assert not box.home.exists(), detail
    assert _home_pickles(box.home) == [], detail
    assert len(_token_posts(box.requests())) <= 1, detail
    assert _no_secrets(proc) == [], detail


def test_nonexistent_home_is_never_created(tmp_path):
    if os.path.lexists("/nonexistent"):
        pytest.skip("this host has /nonexistent")
    box = Box(tmp_path)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE]})
    proc = _run(box, FETCH, extra={"HOME": "/nonexistent"})
    detail = _detail(proc, box)
    assert proc.returncode == 0, detail
    assert box.file("pickle").is_file(), detail
    assert not os.path.lexists("/nonexistent"), detail


def test_second_run_reuses_the_stored_session_with_no_password_login(tmp_path):
    box = Box(tmp_path)
    box.set_routes({"GET " + POSITIONS_CHECK: [EMPTY_PAGE], "GET " + HOLDINGS: [EMPTY_PAGE]})
    first = _run(box, FETCH)
    first_made = box.requests()
    box.reset_log()
    second = _run(box, FETCH)
    second_made = box.requests()
    detail = _detail(second, box) + f"\nfirst:\n{_detail(first, box)}\nfirst requests:\n{first_made}"
    assert first.returncode == 0 and second.returncode == 0, detail
    assert len(_token_posts(first_made)) == 1, detail
    assert len(_token_posts(second_made)) == 0, detail
    checks = [r for r in second_made if r["url"] == POSITIONS_CHECK]
    assert len(checks) == 1 and checks[0]["authorization"] == "Bearer " + STUB_ACCESS, detail
    assert _no_secrets(first) == [] and _no_secrets(second) == [], detail


def test_folder_and_session_files_are_owner_only_under_an_open_umask(tmp_path):
    box = Box(tmp_path)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE]})
    proc = _run(box, FETCH, umask=0)
    detail = _detail(proc, box)
    assert proc.returncode == 0, detail
    assert _mode(box.folder) == 0o700, detail
    for suffix in ("pickle", "device", "lock"):
        assert _mode(box.file(suffix)) == 0o600, (suffix, detail)
        assert os.lstat(box.file(suffix)).st_uid == os.geteuid(), detail
    assert sorted(p.name for p in box.folder.iterdir()) == sorted(
        f"robinhood-{_key(USERNAME)}.{s}" for s in ("device", "lock", "pickle")
    ), detail


def test_an_open_folder_mode_is_tightened_to_owner_only(tmp_path):
    box = Box(tmp_path)
    box.folder.mkdir()
    os.chmod(box.folder, 0o755)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE]})
    proc = _run(box, FETCH)
    detail = _detail(proc, box)
    assert proc.returncode == 0, detail
    assert _mode(box.folder) == 0o700, detail


def test_device_verification_fails_fast_and_latches_until_the_bootstrap(tmp_path):
    box = Box(tmp_path)
    box.set_routes({"POST " + TOKEN_URL: [VERIFICATION]})
    started = time.monotonic()
    proc = subprocess.Popen(
        [sys.executable, str(SCRIPTS / FETCH[0])],
        cwd=str(REPO_ROOT),
        env=box.env(),
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        stdout, stderr = proc.communicate(timeout=30)
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.stdin.close()
    elapsed = time.monotonic() - started
    first = subprocess.CompletedProcess(proc.args, proc.returncode, stdout, stderr)
    first_made = box.requests()
    detail = _detail(first, box)
    assert first.returncode == 1, detail
    assert elapsed < 5, detail
    doc = _doc(first)
    assert doc["error"].startswith(LOGIN_FAILED) and BOOTSTRAP in doc["error"], detail
    assert BOOTSTRAP in first.stderr, detail
    assert len(_token_posts(first_made)) == 1, detail
    assert [r for r in first_made if r["url"].startswith(PATHFINDER)] == [], detail
    assert _mode(box.file("verification")) == 0o600, detail
    assert not box.file("pickle").exists(), detail
    assert _no_secrets(first) == [], detail

    box.reset_log()
    second = _run(box, FETCH)
    detail = _detail(second, box)
    assert second.returncode == 1, detail
    assert box.requests() == [], detail
    second_doc = _doc(second)
    assert BOOTSTRAP in second_doc["error"] and "verification is pending since" in second_doc["error"], detail


class _MarkerPayload:

    def __init__(self, marker):
        self.marker = marker

    def __reduce__(self):
        return (open, (self.marker, "w"))


def _refusal_case(box, case):
    root = box.root
    if case == "unset":
        return {"folder": False}
    if case == "empty":
        return {"folder": ""}
    if case == "relative":
        return {"folder": "relative/robinhood-session"}
    if case == "not_normalized":
        (root / "a").mkdir()
        return {"folder": f"{root}/a/../robinhood-session"}
    if case == "trailing_slash":
        return {"folder": f"{box.folder}/"}
    if case == "symlink_leaf":
        real = root / "real-session"
        real.mkdir(mode=0o700)
        box.folder.symlink_to(real)
        return {}
    if case == "symlink_parent":
        real = root / "real-parent"
        real.mkdir()
        (root / "link-parent").symlink_to(real)
        return {"folder": f"{root}/link-parent/robinhood-session"}
    if case == "missing_parent":
        return {"folder": f"{root}/missing/robinhood-session"}
    if case == "parent_is_a_file":
        (root / "blocker").write_text("")
        return {"folder": f"{root}/blocker/robinhood-session"}
    if case == "read_only_parent":
        parent = root / "read-only"
        parent.mkdir(mode=0o500)
        return {"folder": f"{parent}/robinhood-session"}
    if case == "read_only_folder":
        box.folder.mkdir(mode=0o500)
        return {}
    if case == "session_mode_0644":
        box.seed_session(mode=0o644)
        return {}
    if case == "session_symlink":
        box.folder.mkdir(mode=0o700)
        target = root / "elsewhere.pickle"
        target.write_bytes(b"")
        box.file("pickle").symlink_to(target)
        return {}
    if case == "session_hard_link":
        box.seed_session()
        os.link(box.file("pickle"), root / "second-link.pickle")
        return {}
    if case == "session_with_a_global":
        box.folder.mkdir(mode=0o700)
        fd = os.open(box.file("pickle"), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as f:
            pickle.dump(_MarkerPayload(str(root / "marker")), f)
        return {}
    if case == "session_wrong_keys":
        box.folder.mkdir(mode=0o700)
        fd = os.open(box.file("pickle"), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as f:
            pickle.dump({"token_type": "Bearer", "access_token": STORED_ACCESS}, f)
        return {}
    raise AssertionError(case)


REFUSALS = [
    "unset", "empty", "relative", "not_normalized", "trailing_slash", "symlink_leaf", "symlink_parent",
    "missing_parent", "parent_is_a_file", "read_only_parent", "read_only_folder", "session_mode_0644",
    "session_symlink", "session_hard_link", "session_with_a_global", "session_wrong_keys",
]
ROOT_BYPASSES = ("read_only_parent", "read_only_folder")


@pytest.mark.parametrize("case", REFUSALS)
def test_bad_folder_or_session_refuses_before_any_request(tmp_path, case):
    if case in ROOT_BYPASSES and os.geteuid() == 0:
        pytest.skip("root ignores folder permissions")
    box = Box(tmp_path)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE], "GET " + POSITIONS_CHECK: [EMPTY_PAGE]})
    stored = box.file("pickle").read_bytes() if box.file("pickle").exists() else None
    kwargs = _refusal_case(box, case)
    before = box.file("pickle").read_bytes() if box.file("pickle").is_file() and not box.file("pickle").is_symlink() else stored
    proc = _run(box, FETCH, **kwargs)
    detail = _detail(proc, box)
    assert proc.returncode == 1, detail
    doc = _doc(proc)
    assert doc["error"].startswith(LOGIN_FAILED), detail
    assert box.requests() == [], detail
    assert not box.home.exists(), detail
    assert not (box.root / "marker").exists(), detail
    if before is not None and box.file("pickle").is_file():
        assert box.file("pickle").read_bytes() == before, detail
    assert _no_secrets(proc) == [], detail


def test_parallel_logins_send_one_password_login(tmp_path):
    box = Box(tmp_path)
    box.set_routes({
        "POST " + TOKEN_URL: [dict(TOKEN_OK, sleep_s=1)],
        "GET " + POSITIONS_CHECK: [EMPTY_PAGE],
        "GET " + HOLDINGS: [EMPTY_PAGE],
    })
    procs = [
        subprocess.Popen(
            [sys.executable, str(SCRIPTS / FETCH[0])],
            cwd=str(REPO_ROOT), env=box.env(), stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        for _ in range(2)
    ]
    results = []
    for proc in procs:
        stdout, stderr = proc.communicate(timeout=60)
        results.append(subprocess.CompletedProcess(proc.args, proc.returncode, stdout, stderr))
    made = box.requests()
    detail = "\n".join(_detail(r, box) for r in results)
    assert [r.returncode for r in results] == [0, 0], detail
    posts = _token_posts(made)
    assert len(posts) == 1, detail
    reused = {r["pid"] for r in made if r["url"] == POSITIONS_CHECK}
    assert len(reused) == 1 and reused != {posts[0]["pid"]}, detail
    assert all(_no_secrets(r) == [] for r in results), detail


def test_each_account_keeps_its_own_session(tmp_path):
    box = Box(tmp_path)
    box.set_routes({"GET " + HOLDINGS: [EMPTY_PAGE], "GET " + POSITIONS_CHECK: [EMPTY_PAGE]})
    first = _run(box, FETCH, username="account-a")
    second = _run(box, FETCH, username="account-b")
    made = box.requests()
    detail = _detail(first, box) + _detail(second, box)
    assert first.returncode == 0 and second.returncode == 0, detail
    assert _key("account-a") != _key("account-b"), detail
    assert box.file("pickle", "account-a").is_file() and box.file("pickle", "account-b").is_file(), detail
    assert box.file("device", "account-a").read_text() != box.file("device", "account-b").read_text(), detail
    posts = _token_posts(made)
    assert len(posts) == 2, detail
    assert posts[0]["device_token"] != posts[1]["device_token"], detail


def test_refused_login_keeps_the_stored_session_and_its_device(tmp_path):
    box = Box(tmp_path)
    stored = box.seed_session()
    box.set_routes({
        "GET " + POSITIONS_CHECK: [{"status": 401, "json": {"detail": "expired"}}],
        "POST " + TOKEN_URL: [TOKEN_REFUSED],
    })
    proc = _run(box, FETCH)
    made = box.requests()
    detail = _detail(proc, box)
    assert proc.returncode == 1, detail
    assert box.file("pickle").read_bytes() == stored, detail
    assert _mode(box.file("pickle")) == 0o600, detail
    posts = _token_posts(made)
    assert len(posts) == 1 and posts[0]["device_token"] == STORED_DEVICE, detail
    assert [p.name for p in box.folder.iterdir() if p.name.startswith(".")] == [], detail


def test_device_token_is_stable_across_logins_without_a_session(tmp_path):
    box = Box(tmp_path)
    box.set_routes({"POST " + TOKEN_URL: [TOKEN_REFUSED]})
    first = _run(box, FETCH)
    second = _run(box, FETCH)
    posts = _token_posts(box.requests())
    detail = _detail(first, box) + _detail(second, box)
    assert first.returncode == 1 and second.returncode == 1, detail
    assert len(posts) == 2, detail
    device = box.file("device").read_text().strip()
    assert posts[0]["device_token"] == posts[1]["device_token"] == device, detail
    assert not box.file("pickle").exists(), detail


def test_bootstrap_answers_the_challenge_and_clears_the_latch(tmp_path):
    import pty

    box = Box(tmp_path)
    box.folder.mkdir(mode=0o700)
    latch = box.file("verification")
    fd = os.open(latch, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(json.dumps({"session_sha256": "absent", "since": "2026-10-01T00:00:00Z"}))
    box.set_routes({
        "POST " + TOKEN_URL: [VERIFICATION, TOKEN_OK],
        "POST " + MACHINE: [{"status": 200, "json": {"id": "machine-1"}}],
        "GET " + INQUIRY: [{"status": 200, "json": {"context": {"sheriff_challenge": {
            "type": "sms", "status": "issued", "id": "challenge-1"}}}}],
        "POST " + CHALLENGE: [{"status": 200, "json": {"status": "validated"}}],
        "POST " + INQUIRY: [{"status": 200, "json": {"type_context": {"result": "workflow_status_approved"}}}],
    })
    master, slave = pty.openpty()
    try:
        os.write(master, b"123456\n")
        proc = subprocess.run(
            [sys.executable, str(SCRIPTS / "robinhood_login.py")],
            cwd=str(REPO_ROOT),
            env=box.env(extra={"RH_OFFLINE_STUB_FAST_SLEEP": "1"}),
            stdin=slave,
            capture_output=True,
            text=True,
            timeout=60,
        )
    finally:
        os.close(slave)
        os.close(master)
    made = box.requests()
    detail = _detail(proc, box)
    assert proc.returncode == 0, detail
    assert _mode(box.file("pickle")) == 0o600, detail
    assert not latch.exists(), detail
    assert len(_token_posts(made)) == 2, detail
    assert _count(made, CHALLENGE, "POST") == 1, detail
    assert _no_secrets(proc) == [], detail

    box.reset_log()
    box.set_routes({"GET " + POSITIONS_CHECK: [EMPTY_PAGE], "GET " + HOLDINGS: [EMPTY_PAGE]})
    after = _run(box, FETCH)
    detail = _detail(after, box)
    assert after.returncode == 0, detail
    assert _token_posts(box.requests()) == [], detail


def _run_bootstrap(box):
    import pty

    master, slave = pty.openpty()
    try:
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "robinhood_login.py")],
            cwd=str(REPO_ROOT),
            env=box.env(extra={"RH_OFFLINE_STUB_FAST_SLEEP": "1"}),
            stdin=slave,
            capture_output=True,
            text=True,
            timeout=60,
        )
    finally:
        os.close(slave)
        os.close(master)


def _write_latch(box, identity, mode=0o600):
    fd = os.open(box.file("verification"), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(json.dumps({"session_sha256": identity, "since": "2026-10-01T00:00:00Z"}))
    os.chmod(box.file("verification"), mode)


@pytest.mark.parametrize("latch_mode,check,token,want_exit", [
    pytest.param(0o600, EMPTY_PAGE, None, 0, id="matching_latch_session_accepted"),
    pytest.param(0o644, EMPTY_PAGE, None, 0, id="unreadable_latch_session_accepted"),
    pytest.param(0o600, {"status": 401, "json": {"detail": "expired"}}, TOKEN_REFUSED, 1, id="matching_latch_login_refused"),
])
def test_bootstrap_with_a_stored_session_clears_the_latch_only_on_success(tmp_path, latch_mode, check, token, want_exit):
    box = Box(tmp_path)
    stored = box.seed_session()
    _write_latch(box, hashlib.sha256(stored).hexdigest(), latch_mode)
    routes = {"GET " + POSITIONS_CHECK: [check]}
    if token is not None:
        routes["POST " + TOKEN_URL] = [token]
    box.set_routes(routes)
    proc = _run_bootstrap(box)
    made = box.requests()
    detail = _detail(proc, box)
    assert proc.returncode == want_exit, detail
    assert box.file("pickle").read_bytes() == stored, detail
    assert _no_secrets(proc) == [], detail
    if want_exit != 0:
        assert box.file("verification").exists(), detail
        assert len(_token_posts(made)) == 1, detail
        return
    assert not box.file("verification").exists(), detail
    assert _token_posts(made) == [], detail

    box.reset_log()
    box.set_routes({"GET " + POSITIONS_CHECK: [EMPTY_PAGE], "GET " + HOLDINGS: [EMPTY_PAGE]})
    after = _run(box, FETCH)
    after_made = box.requests()
    detail = _detail(after, box)
    assert after.returncode == 0, detail
    assert _count(after_made, POSITIONS_CHECK) == 1, detail
    assert _token_posts(after_made) == [], detail


def test_bootstrap_refuses_without_a_terminal(tmp_path):
    box = Box(tmp_path)
    proc = _run(box, ["robinhood_login.py"])
    detail = _detail(proc, box)
    assert proc.returncode == 2, detail
    assert box.requests() == [], detail
    assert not box.folder.exists(), detail


def test_a_stalled_lock_holder_is_bounded_by_its_budget(tmp_path):
    box = Box(tmp_path)
    box.seed_session()
    box.set_routes({"GET " + POSITIONS_CHECK: [dict(EMPTY_PAGE, sleep_s=10)]})
    first = _run(box, ["close_robinhood_position.py", "--symbol=BTC", "--mode=live", "--quantity=0.5", "--budget-ms=3000"])
    detail = _detail(first, box)
    assert first.returncode == 1, detail
    assert first.elapsed < 5, detail
    assert _doc(first)["error"].startswith(LOGIN_FAILED), detail
    assert _token_posts(box.requests()) == [], detail

    box.reset_log()
    box.set_routes({"GET " + POSITIONS_CHECK: [EMPTY_PAGE], "GET " + HOLDINGS: [EMPTY_PAGE]})
    second = _run(box, FETCH)
    detail = _detail(second, box)
    assert second.returncode == 0, detail
    assert _token_posts(box.requests()) == [], detail
    assert [p.name for p in box.folder.iterdir() if p.name.startswith(".")] == [], detail
