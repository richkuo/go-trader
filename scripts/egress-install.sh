#!/usr/bin/env bash

set -euo pipefail

THIS_SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${THIS_SCRIPT_DIR}/update_helpers.sh"

usage() {
    cat <<'EOF'
Usage (as root, from the deployment checkout):
  bash scripts/egress-install.sh host --deployment NAME [--unit UNIT] [--status-port N]
        [--notify-user USER] [--notify-destinations discord,telegram,github] [--resolve-seconds N]
  bash scripts/egress-install.sh docker --deployment NAME --env-file FILE [--anchor-container NAME]
        [--worker-uid N] [--worker-gid N] [--status-port N] [--docker-cli PATH]
        [--notify-container NAME --notify-uid N --notify-gid N --notify-env-out FILE]
        [--notify-destinations LIST] [--resolve-seconds N]
  bash scripts/egress-install.sh uninstall --deployment NAME [--unit UNIT]

host: builds the egress controller from this checkout's committed sources (git archive),
installs it root-owned, writes /etc/go-trader/egress/NAME.json, starts
go-trader-egress@NAME, writes the app unit drop-in, and restarts the app unit inside
the enforced namespace. GO_TRADER_EGRESS_PROXY must already be set in the unit's
EnvironmentFile; only its scheme, host and port are copied into the manifest.
docker: same controller and manifest for docker/compose.egress.yaml.
EOF
}

die() {
    echo "[egress-install] ERROR: $*" >&2
    exit 1
}

say() {
    echo "[egress-install] $*"
}

[[ $# -ge 1 ]] || { usage; exit 2; }
mode="$1"
shift
case "$mode" in
    host|docker|uninstall) ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
esac

deployment=""
unit="go-trader.service"
status_port=""
notify_user=""
notify_destinations=""
resolve_seconds=60
env_file=""
anchor_container=""
worker_uid=10001
worker_gid=10001
docker_cli=/usr/bin/docker
notify_container=""
notify_uid=""
notify_gid=""
notify_env_out=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --deployment) deployment="${2:-}"; shift 2 ;;
        --unit) unit="${2:-}"; shift 2 ;;
        --status-port) status_port="${2:-}"; shift 2 ;;
        --notify-user) notify_user="${2:-}"; shift 2 ;;
        --notify-destinations) notify_destinations="${2:-}"; shift 2 ;;
        --resolve-seconds) resolve_seconds="${2:-}"; shift 2 ;;
        --env-file) env_file="${2:-}"; shift 2 ;;
        --anchor-container) anchor_container="${2:-}"; shift 2 ;;
        --worker-uid) worker_uid="${2:-}"; shift 2 ;;
        --worker-gid) worker_gid="${2:-}"; shift 2 ;;
        --docker-cli) docker_cli="${2:-}"; shift 2 ;;
        --notify-container) notify_container="${2:-}"; shift 2 ;;
        --notify-uid) notify_uid="${2:-}"; shift 2 ;;
        --notify-gid) notify_gid="${2:-}"; shift 2 ;;
        --notify-env-out) notify_env_out="${2:-}"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown argument $1" ;;
    esac
done

[[ "$EUID" == "0" ]] || die "run as root: the controller is a root-owned security boundary"
update_egress_valid_deployment "$deployment" || die "--deployment must match ^[a-z0-9][a-z0-9-]{0,30}$"
[[ "$unit" == *.service ]] || unit="$unit.service"
manifest="$UPDATE_EGRESS_MANIFEST_DIR/$deployment.json"
ctl=$(update_egress_controller_path "$deployment")
controller_unit="go-trader-egress@$deployment.service"
broker_unit="go-trader-notify@$deployment.service"

if [[ "$mode" == "uninstall" ]]; then
    if [[ -x "$ctl" && -f "$manifest" ]]; then
        if grep -q '"mode": *"host"' "$manifest"; then
            "$ctl" remove-units --manifest "$manifest"
        fi
    fi
    systemctl daemon-reload
    systemctl disable --now "$broker_unit" 2>/dev/null || true
    if systemctl is-active --quiet "$unit"; then
        say "stopping $unit so the namespace can be removed; it restarts unconfined only if GO_TRADER_EGRESS_PROXY is removed from its environment file"
        systemctl stop "$unit"
    fi
    systemctl disable --now "$controller_unit" 2>/dev/null || true
    if [[ -x "$ctl" && -f "$manifest" ]]; then
        "$ctl" teardown --manifest "$manifest"
    fi
    rm -f "$manifest"
    rm -rf "$UPDATE_EGRESS_LIBEXEC_ROOT/$deployment"
    rm -f "/etc/go-trader/notify-$deployment.env"
    say "removed deployment $deployment. Remove GO_TRADER_EGRESS_PROXY from the app environment file before starting $unit again, or it refuses to start without enforcement."
    exit 0
fi

command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required to write the manifest"
[[ "$resolve_seconds" =~ ^[0-9]+$ ]] || die "--resolve-seconds must be a number"

repo_root=$(update_git "$PWD" rev-parse --show-toplevel) || die "run from the deployment git checkout"
cd "$repo_root"
if ! tree_issues=$(update_foreign_tree_check "$repo_root"); then
    printf '%s\n' "$tree_issues" >&2
    die "$repo_root belongs in part to an account other than root, and root would build the egress controller from it, but the listed units or owners let that account change files in it outside scheduler/ and logs/ (or run without ProtectSystem=strict). Run those units from the go-trader@.service template, whose sandbox keeps the rest of the tree read-only, or give the tree to root"
fi
go_bin=$(update_resolve_tool go) || die "go is required to build the controller"
ver=$(update_git_version "$repo_root") || die "could not read the checkout version"
commit=$(update_git "$repo_root" rev-parse HEAD) || die "could not read HEAD"

say "building the controller from $commit (git archive; untracked and uncommitted files are never built)"
staged=$(update_egress_stage_controller "$deployment" "$repo_root" "$commit" "$go_bin" "$ver" "") || die "controller build failed"
install -o root -g root -m 0755 "$staged" "$ctl.tmp"
mv -f "$ctl.tmp" "$ctl"
rm -rf "$(dirname "$staged")"
say "installed $ctl ($("$ctl" version))"

install -d -o root -g root -m 0755 /etc/go-trader "$UPDATE_EGRESS_MANIFEST_DIR"
[[ -z "$env_file" ]] || env_file=$(cd "$(dirname "$env_file")" && pwd)/$(basename "$env_file")
if [[ -n "$notify_env_out" ]]; then
    notify_env_out=$(cd "$(dirname "$notify_env_out")" && pwd)/$(basename "$notify_env_out")
fi
case "$mode" in
    host)
        [[ -n "$(systemctl show -p FragmentPath --value "$unit" 2>/dev/null)" ]] || die "unit $unit is not installed"
        owner=$(update_unit_owner "$unit") || die "could not resolve the user of $unit"
        worker_uid="${owner%%:*}"
        worker_gid="${owner#*:}"
        [[ "$worker_uid" != "0" ]] || die "$unit runs as root; egress workers must be an unprivileged account (User= in the unit)"
        workdir=$(systemctl show -p WorkingDirectory --value "$unit")
        [[ -n "$workdir" ]] || die "$unit has no WorkingDirectory"
        exec_path=$(systemctl show -p ExecStart --value "$unit" | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n 1)
        [[ -n "$exec_path" ]] || die "could not read the ExecStart path of $unit"
        env_file=""
        env_files=""
        while IFS= read -r entry; do
            [[ -n "$entry" ]] || continue
            path="${entry%% (ignore_errors=*}"
            [[ "$path" == /* ]] || continue
            env_files="$env_files $path"
            if [[ -z "$env_file" && -f "$path" ]] && grep -q '^[[:space:]]*\(export[[:space:]]\+\)\?GO_TRADER_EGRESS_PROXY=' "$path"; then
                env_file="$path"
            fi
        done < <(systemctl show -p EnvironmentFiles --value "$unit")
        [[ -n "$env_file" ]] || die "no EnvironmentFile of $unit sets GO_TRADER_EGRESS_PROXY"
        rw=$(systemctl show -p ReadWritePaths --value "$unit")
        for state_dir in $(systemctl show -p StateDirectory --value "$unit"); do
            rw="$rw /var/lib/$state_dir"
        done
        log_ns=$(systemctl show -p LogNamespace --value "$unit")
        app_config=$(systemctl show -p ExecStart --value "$unit" | sed -n 's/.*argv\[\]=[^;]* --config[ =]\([^ ;]*\).*/\1/p' | head -n 1)
        [[ -n "$app_config" ]] || app_config="scheduler/config.json"
        [[ "$app_config" == /* ]] || app_config="$workdir/$app_config"
        if [[ -z "$status_port" ]]; then
            status_port=$(python3 -c 'import json,sys
try:
    p=json.load(open(sys.argv[1])).get("status_port") or 8099
    print(int(p))
except Exception:
    print(8099)' "$app_config")
        fi
        say "status relay port $status_port (from $app_config)"
        if [[ -n "$notify_user" ]]; then
            say "with notifications_direct, remove DISCORD_BOT_TOKEN, TELEGRAM_BOT_TOKEN and the GitHub token from the app environment files: the scheduler refuses direct mode while it holds them"
            if ! id -u "$notify_user" >/dev/null 2>&1; then
                useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$notify_user"
                say "created system account $notify_user for the notification broker"
            fi
            notify_uid=$(id -u "$notify_user")
            notify_gid=$(id -g "$notify_user")
            [[ "$notify_uid" != "$worker_uid" ]] || die "--notify-user must differ from the worker account"
            "$ctl" extract-notify-env "$env_file" "/etc/go-trader/notify-$deployment.env" "$notify_gid"
        fi
        ;;
    docker)
        [[ -n "$env_file" && -f "$env_file" ]] || die "--env-file must name the compose environment file that holds GO_TRADER_EGRESS_PROXY"
        [[ -x "$docker_cli" ]] || die "docker cli $docker_cli is not executable"
        [[ -n "$anchor_container" ]] || anchor_container="go-trader-egress-net-1"
        [[ -n "$status_port" ]] || status_port=8099
        if [[ -n "$notify_container" ]]; then
            [[ -n "$notify_uid" && -n "$notify_gid" ]] || die "--notify-container needs --notify-uid and --notify-gid"
            [[ -n "$notify_env_out" ]] || die "--notify-container needs --notify-env-out (the broker env file compose mounts)"
            "$ctl" extract-notify-env "$env_file" "$notify_env_out" "$notify_gid"
        fi
        ;;
esac

identity=$("$ctl" identity "$env_file") || die "GO_TRADER_EGRESS_PROXY in $env_file is missing or invalid"
say "proxy identity: $identity (credentials stay in $env_file)"

EGRESS_MODE="$mode" EGRESS_DEPLOYMENT="$deployment" EGRESS_IDENTITY="$identity" EGRESS_WORKER_UID="$worker_uid" \
EGRESS_WORKER_GID="$worker_gid" EGRESS_RESOLVE="$resolve_seconds" EGRESS_STATUS_PORT="$status_port" \
EGRESS_UNIT="$unit" EGRESS_EXEC="${exec_path:-}" EGRESS_WORKDIR="${workdir:-}" EGRESS_ENVFILES="${env_files:-}" \
EGRESS_RW="${rw:-}" EGRESS_LOGNS="${log_ns:-}" EGRESS_DOCKER_CLI="$docker_cli" EGRESS_ANCHOR="$anchor_container" \
EGRESS_NOTIFY_CONTAINER="$notify_container" EGRESS_NOTIFY_UID="${notify_uid:-}" EGRESS_NOTIFY_GID="${notify_gid:-}" \
EGRESS_NOTIFY_DEST="$notify_destinations" python3 - "$manifest.tmp" <<'PY'
import json, os, sys
e = os.environ
m = {
    "deployment": e["EGRESS_DEPLOYMENT"],
    "mode": e["EGRESS_MODE"],
    "proxy": json.loads(e["EGRESS_IDENTITY"]),
    "worker_uid": int(e["EGRESS_WORKER_UID"]),
    "worker_gid": int(e["EGRESS_WORKER_GID"]),
    "resolve_seconds": int(e["EGRESS_RESOLVE"]),
    "status_relays": [{"listen": "127.0.0.1:%d" % int(e["EGRESS_STATUS_PORT"]), "target_port": int(e["EGRESS_STATUS_PORT"])}],
}
if e["EGRESS_MODE"] == "host":
    rw = [p.lstrip("-+") for p in e["EGRESS_RW"].split() if p.lstrip("-+").startswith("/")]
    host = {
        "app_unit": e["EGRESS_UNIT"],
        "app_executable": e["EGRESS_EXEC"],
        "working_directory": e["EGRESS_WORKDIR"],
        "environment_files": e["EGRESS_ENVFILES"].split(),
        "read_write_paths": rw,
    }
    if e["EGRESS_LOGNS"]:
        host["log_namespace"] = e["EGRESS_LOGNS"]
    m["host"] = host
else:
    docker = {"cli": e["EGRESS_DOCKER_CLI"], "anchor_container": e["EGRESS_ANCHOR"]}
    if e["EGRESS_NOTIFY_CONTAINER"]:
        docker["notify_container"] = e["EGRESS_NOTIFY_CONTAINER"]
    m["docker"] = docker
if e["EGRESS_NOTIFY_UID"]:
    notify = {"broker_uid": int(e["EGRESS_NOTIFY_UID"]), "broker_gid": int(e["EGRESS_NOTIFY_GID"])}
    if e["EGRESS_NOTIFY_DEST"]:
        notify["destinations"] = [d.strip() for d in e["EGRESS_NOTIFY_DEST"].split(",") if d.strip()]
    m["notify"] = notify
with open(sys.argv[1], "w") as f:
    json.dump(m, f, indent=2, sort_keys=True)
    f.write("\n")
PY
chown root:root "$manifest.tmp"
chmod 0644 "$manifest.tmp"
mv -f "$manifest.tmp" "$manifest"
say "wrote $manifest"

"$ctl" preflight --manifest "$manifest" || die "preflight failed; nothing was started"

install -o root -g root -m 0644 "$repo_root/systemd/go-trader-egress@.service" /etc/systemd/system/go-trader-egress@.service
systemctl daemon-reload
systemctl enable "$controller_unit" >/dev/null
update_egress_restart_controller "$deployment"

if [[ "$mode" == "docker" ]]; then
    update_egress_wait_answering "$deployment" 60 || die "$controller_unit did not answer on its control socket; see: journalctl -u $controller_unit"
    say "$controller_unit is running; it enforces the namespace once the anchor container $anchor_container starts"
    say "next: in docker/, set GO_TRADER_EGRESS_DEPLOYMENT=$deployment in .env and run: docker compose -f compose.yaml -f compose.egress.yaml up -d (docs/DOCKER.md Egress proxy mode)"
    exit 0
fi

update_egress_wait_ready "$deployment" 60 || die "$controller_unit did not become ready; see: journalctl -u $controller_unit"
say "$controller_unit is ready"

"$ctl" install-units --manifest "$manifest"
systemctl daemon-reload
if [[ -n "$notify_user" ]]; then
    systemctl enable "$broker_unit" >/dev/null
    systemctl restart "$broker_unit"
    say "$broker_unit started"
fi
systemctl restart "$unit"
waited=0
until curl -fsS --max-time 2 "http://127.0.0.1:$status_port/health" >/dev/null 2>&1; do
    if [[ $waited -ge 90 ]]; then
        die "$unit did not answer /health through the status relay within 90s; see: journalctl -u $unit (the namespace stays enforced)"
    fi
    sleep 1
    waited=$((waited + 1))
done
say "$unit runs inside the enforced namespace and answers /health through the status relay on 127.0.0.1:$status_port"
say "manual venue commands now run through: $ctl launch --manifest $manifest -- <command> [args]"
