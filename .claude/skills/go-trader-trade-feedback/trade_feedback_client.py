import argparse
import ipaddress
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

URL_ENV = "GO_TRADER_DASHBOARD_URL"
TOKEN_ENV = "GO_TRADER_STATUS_TOKEN"
PROVIDER = "claude_code"
BUNDLE_VERSION = 1
RESPONSE_LIMIT = 64 << 20
REQUEST_TIMEOUT_S = 60
EXIT_BY_STATUS = {"ok": 0, "invalid": 2, "failed": 3}
REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]
SCHEMA_PATH = REPO_ROOT / "shared_scripts" / "schemas" / "trade_feedback_analysis.schema.json"
VALIDATOR_PATH = REPO_ROOT / "shared_scripts" / "llm_strategy_feedback.py"


class ClientError(Exception):
    pass


class RefuseRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), RefuseRedirect())


def is_loopback(host):
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def dashboard_origin():
    raw = os.environ.get(URL_ENV, "").strip()
    if not raw:
        raise ClientError(f"set {URL_ENV} to the dashboard origin, for example http://127.0.0.1:8099")
    parts = urllib.parse.urlsplit(raw)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise ClientError(f"{URL_ENV} must be an http or https origin")
    if parts.username is not None or parts.password is not None:
        raise ClientError(f"{URL_ENV} must not hold a user name or password")
    if parts.path not in ("", "/") or parts.query or parts.fragment:
        raise ClientError(f"{URL_ENV} must be an origin only, with no path, query or fragment")
    try:
        port = parts.port
    except ValueError:
        raise ClientError(f"{URL_ENV} has an invalid port")
    if parts.scheme == "http" and not is_loopback(parts.hostname):
        raise ClientError(f"{URL_ENV} may use http only for a loopback host; use an SSH tunnel to 127.0.0.1 or an https origin")
    host = parts.hostname
    if ":" in host:
        host = f"[{host}]"
    return f"{parts.scheme}://{host}" + (f":{port}" if port else "")


def status_token():
    token = os.environ.get(TOKEN_ENV, "").strip()
    if not token:
        raise ClientError(f"set {TOKEN_ENV} in this shell; this client never prints it")
    if any(c in token for c in "\r\n\0"):
        raise ClientError(f"{TOKEN_ENV} holds a line break or NUL character")
    return token


def scrub(text):
    token = os.environ.get(TOKEN_ENV, "").strip()
    if token:
        text = text.replace(token, "[redacted]")
    return text


def strategy_path(strategy_id, resource):
    if not strategy_id or strategy_id != strategy_id.strip():
        raise ClientError("the strategy id is empty or has surrounding spaces")
    return "/api/strategies/" + urllib.parse.quote(strategy_id, safe="") + "/trade-feedback/" + resource


def call(method, path, body=None):
    origin = dashboard_origin()
    token = status_token()
    req = urllib.request.Request(origin + path, data=body, method=method)
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("Accept", "application/json")
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with OPENER.open(req, timeout=REQUEST_TIMEOUT_S) as resp:
            status, raw = resp.status, resp.read(RESPONSE_LIMIT + 1)
    except urllib.error.HTTPError as exc:
        status, raw = exc.code, exc.read(RESPONSE_LIMIT + 1)
    except urllib.error.URLError as exc:
        raise ClientError(scrub(f"cannot reach {origin}: {exc.reason}"))
    except OSError as exc:
        raise ClientError(scrub(f"cannot reach {origin}: {exc}"))
    if 300 <= status < 400:
        raise ClientError(f"the server answered {status} with a redirect; this client never follows redirects")
    if len(raw) > RESPONSE_LIMIT:
        raise ClientError(f"the response is larger than {RESPONSE_LIMIT} bytes")
    return status, raw


def server_error(status, raw):
    text = raw.decode("utf-8", "replace")
    try:
        parsed = json.loads(text)
        if isinstance(parsed, dict) and isinstance(parsed.get("error"), str):
            text = parsed["error"]
    except ValueError:
        pass
    return ClientError(scrub(f"HTTP {status}: {text.strip()[:2000]}"))


def emit(obj):
    print(scrub(json.dumps(obj, indent=2, sort_keys=True)))


def read_bundle(path):
    raw = pathlib.Path(path).read_text(encoding="utf-8")
    try:
        bundle = json.loads(raw)
    except ValueError as exc:
        raise ClientError(f"the bundle file is not valid JSON: {exc}")
    if not isinstance(bundle, dict) or bundle.get("bundle_version") != BUNDLE_VERSION:
        raise ClientError(f"the bundle file is not a version {BUNDLE_VERSION} trade feedback bundle")
    digest = bundle.get("bundle_sha256")
    ids = (bundle.get("selection") or {}).get("feedback_rowids")
    if not isinstance(digest, str) or len(digest) != 64 or not isinstance(ids, list) or not all(isinstance(i, int) and not isinstance(i, bool) for i in ids):
        raise ClientError("the bundle file has no bundle_sha256 or selection.feedback_rowids")
    return raw, bundle, digest, ids


def read_result(path):
    raw = pathlib.Path(path).read_text(encoding="utf-8").strip()
    try:
        parsed = json.loads(raw)
    except ValueError as exc:
        raise ClientError(f"the result file is not valid JSON: {exc}")
    if not isinstance(parsed, dict):
        raise ClientError("the result must be one JSON object")
    return raw


def cmd_pull(args):
    status, raw = call("GET", strategy_path(args.strategy_id, "bundle"))
    if status != 200:
        raise server_error(status, raw)
    out_dir = pathlib.Path(args.out) if args.out else pathlib.Path(tempfile.mkdtemp(prefix="go-trader-trade-feedback-"))
    out_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = out_dir / "bundle.json"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(raw)
    _, bundle, digest, ids = read_bundle(path)
    strategy = bundle.get("strategy") or {}
    if strategy.get("strategy_id") != args.strategy_id:
        raise ClientError("the bundle names a different strategy than the one requested")
    emit({
        "bundle_file": str(path),
        "work_dir": str(out_dir),
        "strategy_id": strategy.get("strategy_id"),
        "partition": strategy.get("partition"),
        "bundle_sha256": digest,
        "feedback_rowids": ids,
        "marks": len(bundle.get("marks") or []),
        "exclusions": len((bundle.get("selection") or {}).get("exclusions") or []),
        "result_schema": str(SCHEMA_PATH),
    })
    return 0


def cmd_check(args):
    bundle_raw, _, _, _ = read_bundle(args.bundle)
    result_raw = read_result(args.result)
    uv = shutil.which("uv")
    if uv is None:
        raise ClientError("uv is not installed, so the local pre-check cannot run; the server still validates every submission")
    stdin = '{"bundle":' + bundle_raw.strip() + ',"result":' + json.dumps(result_raw) + "}"
    proc = subprocess.run([uv, "run", "--no-sync", "python", str(VALIDATOR_PATH), "--validate-only"], input=stdin,
                          capture_output=True, text=True, cwd=REPO_ROOT, timeout=300)
    try:
        out = json.loads(proc.stdout)
    except ValueError:
        raise ClientError(f"the local validator failed: {proc.stderr.strip()[:2000]}")
    emit(out)
    if out.get("outcome") == "ok":
        return 0
    return 2


def cmd_submit(args):
    _, bundle, digest, ids = read_bundle(args.bundle)
    if (bundle.get("strategy") or {}).get("strategy_id") != args.strategy_id:
        raise ClientError("the bundle file belongs to a different strategy")
    result_raw = read_result(args.result)
    model = args.model.strip()
    if not model:
        raise ClientError("--model must name the model this session runs on")
    body = ('{"provider":' + json.dumps(PROVIDER) + ',"model":' + json.dumps(model) + ',"bundle_sha256":' + json.dumps(digest) +
            ',"feedback_rowids":' + json.dumps(ids) + ',"result":' + result_raw + "}")
    status, raw = call("POST", strategy_path(args.strategy_id, "analyses"), body.encode("utf-8"))
    if status != 202:
        raise server_error(status, raw)
    resp = json.loads(raw.decode("utf-8"))
    emit({"request_id": resp.get("request_id"), "status": resp.get("status"), "bundle_sha256": resp.get("bundle_sha256"),
          "feedback_rowids": resp.get("feedback_rowids")})
    return 0


def cmd_poll(args):
    deadline = time.monotonic() + args.timeout
    while True:
        status, raw = call("GET", strategy_path(args.strategy_id, "analyses") + "?view=history")
        if status != 200:
            raise server_error(status, raw)
        data = json.loads(raw.decode("utf-8"))
        match = next((a for a in data.get("analyses") or [] if a.get("request_id") == args.request_id), None)
        if match is None:
            raise ClientError(f"request {args.request_id} is not in the newest analyses of this strategy")
        if match.get("status") in EXIT_BY_STATUS:
            validation = match.get("validation") or {}
            result = match.get("result") or {}
            emit({
                "request_id": match.get("request_id"),
                "status": match.get("status"),
                "model": match.get("model"),
                "model_verification": match.get("model_verification"),
                "error": match.get("error") or "",
                "validation_reasons": validation.get("reasons") or [],
                "proposals": validation.get("proposals") or [],
                "summary": result.get("summary") if isinstance(result, dict) else None,
                "completed_at": match.get("completed_at"),
            })
            return EXIT_BY_STATUS[match["status"]]
        if time.monotonic() >= deadline:
            raise ClientError(f"request {args.request_id} is still {match.get('status')} ({match.get('progress', '')}) after {args.timeout}s")
        time.sleep(args.interval)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Pull a go-trader trade feedback bundle, submit a local analysis and poll its outcome.")
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("pull")
    p.add_argument("strategy_id")
    p.add_argument("--out")
    p.set_defaults(func=cmd_pull)
    c = sub.add_parser("check")
    c.add_argument("--bundle", required=True)
    c.add_argument("--result", required=True)
    c.set_defaults(func=cmd_check)
    s = sub.add_parser("submit")
    s.add_argument("strategy_id")
    s.add_argument("--bundle", required=True)
    s.add_argument("--result", required=True)
    s.add_argument("--model", required=True)
    s.set_defaults(func=cmd_submit)
    q = sub.add_parser("poll")
    q.add_argument("strategy_id")
    q.add_argument("request_id")
    q.add_argument("--interval", type=float, default=3.0)
    q.add_argument("--timeout", type=float, default=960.0)
    q.set_defaults(func=cmd_poll)
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except ClientError as exc:
        print(scrub(f"error: {exc}"), file=sys.stderr)
        return 1
    except (OSError, ValueError) as exc:
        print(scrub(f"error: {exc}"), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
