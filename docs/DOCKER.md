# Run go-trader with Docker

This guide runs go-trader in a Docker container on macOS, Windows or Linux.
You do not need Go, Python or uv on your computer.
On a Linux server, the systemd service in [SKILL.md](../SKILL.md) stays the main production setup; the Docker setup is an option.

The container starts with **paper trading only**: it trades with simulated money.
Read the whole guide before you put real money behind it.

## Contents

- [What you get](#what-you-get)
- [Requirements](#requirements)
- [Install Docker](#install-docker)
- [Get the files](#get-the-files)
- [Secrets](#secrets)
- [First setup](#first-setup)
- [Start, stop and status](#start-stop-and-status)
- [Dashboard](#dashboard)
- [Logs](#logs)
- [Change the config and hot reload](#change-the-config-and-hot-reload)
- [Utilities](#utilities)
- [Run one cycle with --once](#run-one-cycle-with---once)
- [Restarts, health and fatal exits](#restarts-health-and-fatal-exits)
- [Start after login or reboot](#start-after-login-or-reboot)
- [Backup](#backup)
- [Restore](#restore)
- [Upgrade](#upgrade)
- [Roll back](#roll-back)
- [Image identity](#image-identity)
- [Volume permissions](#volume-permissions)
- [Live trading](#live-trading)
- [Build the image yourself](#build-the-image-yourself)
- [Troubleshooting](#troubleshooting)

## What you get

| Part | Where | Notes |
| --- | --- | --- |
| Image | `ghcr.io/richkuo/go-trader:<release>` | One image holds the Go binary and the Python scripts of one source commit. Images exist for `linux/amd64` and `linux/arm64`, for releases published after the Docker setup was added. |
| Service `go-trader` | `docker/compose.yaml` | The long-running bot. It restarts by itself after a crash or an internal restart. |
| Service `cli` | `docker/compose.yaml`, profile `cli` | One-shot commands: setup, probe, inspect, manual trades, `--once`. It never restarts by itself. |
| Data volume | `go-trader_data`, mounted at `/data` | The config (`/data/config.json`), every state database and its sidecar files, lock files, tuning runs and the OHLCV cache. |
| Log volume | `go-trader_logs`, mounted at `/app/logs` | Strategy log files. The bot's main output goes to `docker compose logs`. |
| Secrets file | `docker/go-trader.env` | Tokens and exchange keys. It stays on your computer and never goes into an image. |
| Dashboard | `http://127.0.0.1:8099/dashboard` | Only your own computer can open it, and every data request needs your token. |

The container runs as user id 10001, with a read-only code directory and no extra privileges.
The bot can write only to `/data`, `/app/logs` and `/tmp`.

## Requirements

- **Docker Engine 28.0.0 or newer.** Docker Desktop includes Docker Engine. Check with `docker version` (look at `Server: ... Version`).
  Before Engine 28.0.0, other computers on the same local network could sometimes reach a port that was published only on `127.0.0.1` ([Docker port publishing](https://docs.docker.com/engine/network/port-publishing/)).
- **Docker Compose 2.24 or newer.** Check with `docker compose version`.
- **Default Docker networking.** The setup publishes the dashboard port on `127.0.0.1` with Docker's normal port forwarding.
  Do not use the direct routing modes (`gateway_mode_ipv4: routed` or `nat-unprotected`), `--iptables=false`, or a network that routes to container addresses from other hosts; with these, other computers can reach the container's address directly.
- About 2 GB of free disk space for the image.
- A network connection to the exchanges your strategies use.

## Install Docker

**macOS.** Install [Docker Desktop for Mac](https://docs.docker.com/desktop/setup/install/mac-install/). Pick the Apple silicon or the Intel download for your Mac. Start Docker Desktop once and wait until it says the engine is running.

**Windows.** Install [Docker Desktop for Windows](https://docs.docker.com/desktop/setup/install/windows-install/) with the WSL 2 backend. Start Docker Desktop once and wait until it says the engine is running. Use PowerShell or Windows Terminal for the commands in this guide.

**Linux.** Install [Docker Engine](https://docs.docker.com/engine/install/) and the Compose plugin for your distribution. Add your account to the `docker` group, or put `sudo` before each `docker` command.

Check the install:

```bash
docker version
docker compose version
```

## Get the files

You need the `docker` folder of this repository. The simplest way is a Git clone:

```bash
git clone https://github.com/richkuo/go-trader.git
cd go-trader/docker
```

Run every command in this guide from the `go-trader/docker` folder.
The repository keeps line endings for scripts and config files as LF, so a Windows clone also works.

## Secrets

The bot reads tokens and keys from `docker/go-trader.env`. Make it from the example:

```bash
cp go-trader.env.example go-trader.env
```

`cp` also works in Windows PowerShell.

Then open `go-trader.env` in a text editor and set `STATUS_AUTH_TOKEN`. It is required: the bot refuses to start without it.
Use a long random value. To make one:

```bash
openssl rand -hex 32
```

In Windows PowerShell:

```powershell
$b = New-Object byte[] 32; [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); ($b | ForEach-Object { $_.ToString('x2') }) -join ''
```

Write one value per line as `NAME=value`, with no quotes and no spaces around `=`. Leave a line empty when you do not use it:

| Variable | Use |
| --- | --- |
| `STATUS_AUTH_TOKEN` | Required. The dashboard and every status request need it. |
| `DISCORD_BOT_TOKEN`, `DISCORD_OWNER_ID` | Discord notifications and owner commands. |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_OWNER_CHAT_ID` | Telegram notifications. |
| `HYPERLIQUID_SECRET_KEY`, `HYPERLIQUID_ACCOUNT_ADDRESS`, `OKX_*`, `TOPSTEP_*`, `ROBINHOOD_*`, `LUNO_*` | Exchange keys for live trading only. Paper trading needs none. |
| `ANTHROPIC_API_KEY` | The optional LLM entry analysis. |

On macOS and Linux, make the file private: `chmod 600 go-trader.env`.
Git and the Docker build both ignore `go-trader.env`, so it never goes into a commit or an image.
Back it up in a password manager or another protected place, separate from the data backups below.
A change to `go-trader.env` takes effect when the container is created again: `docker compose up -d --force-recreate go-trader`.

## First setup

1. **Pick a release.** Open the [releases page](https://github.com/richkuo/go-trader/releases) and pick a release that has a container image. Make the Compose settings file and set the tag:

   ```bash
   cp env.example .env
   ```

   Open `.env` and set, for example, `GO_TRADER_TAG=v0.109.0`. The tag stays fixed until you upgrade on purpose.

2. **Download the image.**

   ```bash
   docker compose pull go-trader
   ```

3. **Make the config.** The setup asks questions; press Enter to accept a default.

   ```bash
   docker compose run --rm cli init
   ```

   The setup writes `/data/config.json` in the data volume, puts the state database at `/data/state.db`, keeps automatic updates off, and creates paper strategies only.
   It does not ask for a file location: in the container the config always goes in the data volume.

   To skip the questions, give the answers as JSON instead:

   ```bash
   docker compose run --rm cli init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}'
   ```

   Quoting JSON on a command line differs between shells, so on Windows use the questions above.
   The JSON setup refuses live modes, manual trading, an `autoUpdate` value other than `off`, and an `--output` path outside `/data`.

4. **Check the config.**

   ```bash
   docker compose run --rm cli probe --config /data/config.json
   ```

   It prints `probe: OK (...)` and exits 0.

The first run of a new volume makes it ready automatically: Docker copies the empty `/data` folder of the image, which belongs to user 10001, into the new volume. You do not change any owner by hand.

## Start, stop and status

| Task | Command |
| --- | --- |
| Start in the background | `docker compose up -d` |
| Show state and health | `docker compose ps` |
| Stop (state is saved) | `docker compose stop` |
| Start again | `docker compose start` |
| Restart | `docker compose restart go-trader` |
| Remove the containers, keep all data | `docker compose down` |

`docker compose ps` shows `(healthy)` when the bot answers its health check.
A stop sends SIGTERM: the bot finishes or cancels its running checks, saves its state, and exits within 20 seconds.

**Never run `docker compose down -v`** unless you want to delete the config and every state database. The `-v` removes the volumes.

## Dashboard

Open [http://127.0.0.1:8099/dashboard](http://127.0.0.1:8099/dashboard) in a browser on the same computer.
The page asks for your token: paste the `STATUS_AUTH_TOKEN` value. The browser keeps it for this site.

Only your own computer can open the page, because Compose publishes the port on `127.0.0.1` only.
Inside Docker the bot listens on all container addresses so that the port can be published, and for that reason it refuses to start without a token.
The health endpoint (`/health`) answers without a token and shows only the status, version and process id.

To read the status from a terminal:

```bash
curl -H "Authorization: Bearer <your token>" http://127.0.0.1:8099/status
```

Port 8099 already in use? Set `GO_TRADER_HOST_PORT=8100` (or another free port) in `.env` and run `docker compose up -d` again.

## Logs

```bash
docker compose logs -f go-trader            # follow the bot output; Ctrl+C stops following
docker compose logs --tail 200 go-trader    # the last 200 lines
```

The Discord `/logs` command replies with this `docker compose logs` command, because the container has no system journal.
Per-strategy log files are in the log volume:

```bash
docker compose run --rm --entrypoint ls cli -la /app/logs
```

## Change the config and hot reload

The dashboard and the Discord config commands change the config for you, with checks.
To edit the file yourself:

1. Copy it out of the volume:

   ```bash
   docker compose cp go-trader:/data/config.json ./config.json
   ```

2. Edit `./config.json` in a text editor.

3. Copy it back, then give it back to the bot's user (the copy arrives owned by your host user, which the bot cannot read):

   ```bash
   docker compose cp ./config.json go-trader:/data/config.json
   docker compose run --rm --user 0:0 --cap-add CHOWN --cap-add DAC_READ_SEARCH --entrypoint chown cli 10001:10001 /data/config.json
   ```

4. Check it:

   ```bash
   docker compose run --rm cli probe --config /data/config.json
   ```

5. Apply it without a restart:

   ```bash
   docker compose exec go-trader sh -c 'kill -HUP 1'
   docker compose logs --tail 30 go-trader
   ```

   This sends SIGHUP to the container's init process, which passes it to the supervisor and then to the bot.
   The log shows `[reload]` lines. A change that needs a restart (for example a new or removed strategy) is refused and the bot keeps its running config; apply it with `docker compose restart go-trader`.

   `docker compose kill -s HUP go-trader` also reaches the bot, but Docker then counts the container as stopped by hand: after the next exit (a crash or an internal restart) it does **not** start the container again.
   If you used it, run `docker compose restart go-trader` afterwards to turn the restart policy back on.

The container accepts only `"auto_update": "off"` (or no `auto_update` key). A config with `daily` or `heartbeat` is refused at start, by `probe`, by a reload and by a config command.

## Utilities

Every utility runs in the `cli` service, and each one needs `--config /data/config.json`.
The one-shot route returns the real exit code of the command, and nothing restarts it.

```bash
docker compose run --rm cli probe --config /data/config.json
docker compose run --rm cli inspect --config /data/config.json --all
docker compose run --rm cli storage-inspect --config /data/config.json
```

`inspect`, `storage-inspect` and `probe` only read, so they run beside the running bot. Other commands in `go-trader agent-info` take the same `--config /data/config.json` argument.

**Manual Hyperliquid trades** (these need live Hyperliquid keys in `go-trader.env` and a manual strategy; see [Live trading](#live-trading)):

```bash
docker compose run --rm cli manual-open hl-manual-btc --config /data/config.json --side long --margin 50
docker compose run --rm cli manual-update-sl hl-manual-btc --config /data/config.json --trigger 66000
docker compose run --rm cli manual-close hl-manual-btc --config /data/config.json
```

Manual commands take only the manual-action lock, so they run beside the running bot, as on a host.
The full list of manual commands is in [SKILL.md](../SKILL.md) § Manual Trading.

## Run one cycle with --once

`--once` runs one full trading cycle and exits. It takes the same ownership locks as the bot, so stop the bot first:

```bash
docker compose stop go-trader
docker compose run --rm cli --config /data/config.json --once
docker compose start go-trader
```

While the bot runs, `--once` refuses with exit code 79.

## Restarts, health and fatal exits

The `go-trader` service runs a small supervisor (`go-trader supervise`) as its main process, behind Docker's init process.
The supervisor starts one bot, passes SIGTERM, SIGINT and SIGHUP to it, and exits with the bot's exit code.
The service uses `restart: unless-stopped`.

| What happens | Result |
| --- | --- |
| The bot crashes after a healthy start | Docker starts the container again. |
| Dashboard restart, Discord `/restart`, or a config change that needs a restart | The bot saves its state and exits 0; Docker starts the container again. These never use `systemctl`. |
| The bot fails during start (for example a config error, exit 1) | Docker starts the container again after a short wait that grows with each failure. Read `docker compose logs` and fix the cause. |
| Exit 78 (a check-script probe failed), 79 (another process owns the state files) or 80 (the storage layout is wrong) | The supervisor writes one `[supervisor] CRITICAL` line and **holds**: it does not start the bot again, the container stays running, and `docker compose ps` shows `unhealthy`. |
| `docker compose stop` | The container stops and stays stopped, also after Docker restarts. |
| `docker compose kill` with any signal, including `-s HUP` | Docker counts the container as stopped by hand: the next exit does not restart it. Run `docker compose restart go-trader` to turn the restart policy back on. |

To recover from a hold:

1. Read the bot's own error lines just above the CRITICAL line: `docker compose logs --tail 100 go-trader`.
2. Fix the cause. For 78, check that `GO_TRADER_TAG` names a published release image and that no strategy names a script that is not in the image. For 79, stop the other container or `cli` run that uses the same data volume (`docker compose ps -a`). For 80, see [SKILL.md](../SKILL.md) § Storage Ownership.
3. Run `docker compose restart go-trader`.

`docker compose stop` also ends a held container.

`(healthy)` means the bot answers and its main loop keeps its own schedule.
The status turns `unhealthy` during a fatal-exit hold, when one pass of the loop runs longer than 30 minutes, or when the loop does not wake up within 5 minutes of its planned time.
A long `interval_seconds` (for example one hour between cycles) does not make it unhealthy.

## Start after login or reboot

The container starts again whenever Docker starts, unless you stopped it with `docker compose stop` (or `docker stop`).

- **macOS and Windows:** Docker Desktop runs only after you sign in. To start the bot after a reboot, turn on Docker Desktop → Settings → General → **Start Docker Desktop when you sign in to your computer**, then sign in. Nothing runs while the computer is at the sign-in screen, asleep or shut down.
- **Linux:** run `sudo systemctl enable docker` so that Docker starts at boot.

After a reboot, check with `docker compose ps`.

## Backup

Back up the data volume while the bot is **stopped**, so the config and every database are saved together at one point in time.

1. Stop the bot and make sure that nothing else uses the volume:

   ```bash
   docker compose stop
   docker compose ps -a
   docker compose logs --tail 20 go-trader
   ```

   `ps -a` must show no running `go-trader` or `cli` container, and the log must end with `[shutdown] State saved.` and `[shutdown] Complete.`

2. Copy the whole volume out, with a record of the image that wrote it:

   ```bash
   mkdir -p backups/2026-10-07
   docker compose cp go-trader:/data ./backups/2026-10-07/data
   docker compose run --rm cli version --json > ./backups/2026-10-07/image.json
   ```

   Use the date (or another name) for the folder. In Windows PowerShell, use `mkdir backups/2026-10-07` for the first line.
   `image.json` records the release and the source commit of the image.

3. Start the bot again: `docker compose start`.

The copy holds `config.json`, every state database with its `-wal` and `-shm` files, the lock files, `tuning_runs/` and the OHLCV cache.
Keep `go-trader.env` in its own protected backup; this backup does not contain it.

## Restore

Restore only while every writer is stopped.
The same steps work with a stopped container, after `docker compose down`, and on a new computer that has only the `docker` folder files and the backup.

```bash
docker compose stop
docker compose ps -a
docker compose create go-trader
docker compose run --rm --entrypoint find cli /data -mindepth 1 -delete
docker compose cp ./backups/2026-10-07/data/. go-trader:/data
docker compose run --rm --user 0:0 --cap-add CHOWN --cap-add DAC_READ_SEARCH --entrypoint chown cli -R 10001:10001 /data
docker compose run --rm cli probe --config /data/config.json
docker compose start
```

`docker compose create` makes the bot's container (and the volumes, when they do not exist) without starting it, so that the copy has a target; it changes nothing when the stopped container already exists. It runs before `find`, so a failure stops the restore before anything is deleted.
The `find` command deletes the current content of the data volume. The `chown` command gives the restored files back to the bot's user; it is safe because the volume holds only go-trader files.
Use the image release recorded in the backup's `image.json`, or a newer one (see [Roll back](#roll-back)).

## Upgrade

The container never updates itself: the `auto_update` setting must stay `off`, and a "yes" to an update message replies with these steps instead.
An upgrade changes the image, and with it the Go binary and the Python scripts together.

1. Read the release notes of the new release.
2. Download the new image without changing the running setup:

   ```bash
   docker pull ghcr.io/richkuo/go-trader:<new tag>
   ```

3. Stop the bot and every other user of the data volume, and confirm the state save, as in [Backup](#backup) step 1.
4. Take a backup, as in [Backup](#backup) step 2. Keep it until the new release has run well for some time.
5. Set `GO_TRADER_TAG=<new tag>` in `.env`.
6. Check the new image before you start it:

   ```bash
   docker compose run --rm cli version --json
   docker compose run --rm cli probe --config /data/config.json
   docker compose run --rm cli storage-inspect --config /data/config.json
   ```

   `version --json` must show the new release and the release's source commit. `probe` must exit 0, and `storage-inspect` must report no problem.
7. Start it and check it:

   ```bash
   docker compose up -d
   docker compose ps
   docker compose logs --tail 100 go-trader
   ```

   Wait for `(healthy)`. The log shows `[storage]` lines for every state file. The dashboard shows your paper positions and history from before the upgrade.

## Roll back

A new release can change the database layout when it first opens the files. An older image cannot be trusted with files that a newer image changed.
So a rollback goes back to the old image **together with** the backup taken just before the upgrade.

**No live trading since the upgrade** (paper trading only, or live strategies that placed no order):

1. `docker compose stop`
2. Set `GO_TRADER_TAG` in `.env` back to the old tag (the release in the backup's `image.json`).
3. [Restore](#restore) the pre-upgrade backup.
4. `docker compose up -d`

Paper trades made after the backup are lost.

**Live orders, fills or position changes since the backup:** do **not** resume trading from the restored book.
The exchange now holds positions and orders that the old book does not know, so the bot could act on wrong positions.

1. Keep the bot stopped.
2. Get current evidence from the exchange: open positions, open orders, and fills since the backup time (the exchange web page, or the Hyperliquid fill history).
3. Reconcile the saved records with that evidence through the supported operator procedures in [SKILL.md](../SKILL.md): § Manual Trading (record-only opens and closes), § Backfill Trade Ledger and § Model-Only Close Reconciliation. When the newer release ran correctly, the better choice is often to stay on it and fix forward.
4. Start trading only when the book and the exchange agree.

A downgrade of the image alone is never a rollback.

## Image identity

```bash
docker compose run --rm cli version --json
docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' ghcr.io/richkuo/go-trader:<tag>
```

`version --json` prints the release (`version`) and the source commit (`source_commit`) that were built into the binary. The image label shows the same commit. The release workflow checks that both match before it publishes an image.
`--version` alone prints only the release.

## Volume permissions

The bot runs as user 10001 and needs to write `/data` and `/app/logs`.
A new, empty volume gets the right owner by itself.
An existing volume with other owners (for example one that an older setup or another program wrote) makes the bot refuse to start:

```
[supervisor] CRITICAL: refusing to start the daemon: /data (config and state databases) is not writable by uid 10001 ...
```

The container then holds as unhealthy and starts nothing. `init` refuses with the same message.

To fix it, first look at what the volume holds:

```bash
docker run --rm -v go-trader_data:/data --entrypoint ls ghcr.io/richkuo/go-trader:<tag> -lan /data
```

Use the data volume only for go-trader. When it holds only go-trader files (`config.json`, `*.db` files and their sidecars, `tuning_runs`, `ohlcv_cache.sqlite3`), give it to user 10001:

```bash
docker run --rm --user 0:0 -v go-trader_data:/data --entrypoint chown ghcr.io/richkuo/go-trader:<tag> -R 10001:10001 /data
docker run --rm --user 0:0 -v go-trader_data:/data --entrypoint chmod ghcr.io/richkuo/go-trader:<tag> 0700 /data
```

When it holds files from another program, do not change their owner. Move go-trader to a new volume instead: back up its files as in [Backup](#backup), then restore them into a new, empty volume.
The same steps apply to `go-trader_logs` mounted at `/app/logs`.

A missing volume also stops the start: `/data is not a mounted volume`. Use the commands in this guide, which always mount both volumes.

## Live trading

The first setup creates paper strategies only.
To trade with real money later, change a strategy to live mode in the config (see [SKILL.md](../SKILL.md) § Configure), put the exchange keys in `go-trader.env`, run `docker compose up -d --force-recreate go-trader`, and watch the first cycles.
Every database must stay in `/data`: the container refuses a config whose `db_file`, `paper_db_file`, `paper_sources[].db_file` or `replay_log_path` points somewhere else.
The container does not support `role: "feed"` services or `market_feed: "shared"`.
Understand the [Roll back](#roll-back) limits before you trade live.

## Build the image yourself

For development, build the image from your checkout with the build override:

```bash
GO_TRADER_TAG=local docker compose -f compose.yaml -f compose.build.yaml build
GO_TRADER_TAG=local docker compose -f compose.yaml -f compose.build.yaml up -d
```

Set `GO_TRADER_BUILD_VERSION` and `GO_TRADER_BUILD_COMMIT` (a full 40-character commit id) to stamp the build.
The build uses only the files listed in `.dockerignore` and copies no config, database, log, secret or Git file into the image.
To test an image, run `scripts/test_container_image.sh` (see the script for its variables); CI runs it on `linux/amd64` and `linux/arm64`.

## Troubleshooting

| Message or symptom | Cause and fix |
| --- | --- |
| `Set GO_TRADER_TAG in docker/.env ...` | `.env` is missing or `GO_TRADER_TAG` is empty. See [First setup](#first-setup) step 1. |
| `env file ... go-trader.env not found` | Make `go-trader.env` from the example. See [Secrets](#secrets). |
| `STATUS_AUTH_TOKEN is empty` in the log, and the container keeps restarting | Set the token in `go-trader.env`, then `docker compose up -d --force-recreate go-trader`. |
| `Failed to load config: read config: open /data/config.json: no such file or directory` | Run the [First setup](#first-setup). |
| `auto_update is "daily"` (or `"heartbeat"`) | Set `auto_update` to `off` or remove it. See [Change the config and hot reload](#change-the-config-and-hot-reload). |
| `(unhealthy)` and a `[supervisor] CRITICAL` line | A fatal exit hold. See [Restarts, health and fatal exits](#restarts-health-and-fatal-exits). |
| `not writable by uid 10001` | See [Volume permissions](#volume-permissions). |
| `--once` exits 79 | The bot is running. Stop it first. |
| The dashboard shows "Authorization required" | Paste the `STATUS_AUTH_TOKEN` value in the token field. |
| `port is already allocated` | Set `GO_TRADER_HOST_PORT` in `.env` to a free port. |
| `status_port is 8100, but the container serves the dashboard and the health check on port 8099` | Remove `status_port` from the config or set it to 8099. To use another port on your computer, set `GO_TRADER_HOST_PORT` in `.env`. See [Change the config and hot reload](#change-the-config-and-hot-reload). |
