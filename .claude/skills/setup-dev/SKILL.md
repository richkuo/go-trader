---
name: setup-dev
description: Set up development environment for go-trader. Use when onboarding or after a fresh clone.
disable-model-invocation: true
---

# Dev Environment Setup: go-trader

## 1. Install Go 1.26.0

**macOS (Homebrew):**
```bash
brew install go@1.26
# or upgrade if already installed:
brew upgrade go
```

Go binary will be at `/opt/homebrew/bin/go`. It is not added to PATH automatically — use the full path for all Go commands.

**Linux:**
```bash
# Download and install manually (recommended for exact version control)
wget https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
```

Go binary will be at `/usr/local/go/bin/go`. Add to PATH or use the full path:
```bash
export PATH=$PATH:/usr/local/go/bin
```

**Verify:**
```bash
# macOS
/opt/homebrew/bin/go version
# expected: go version go1.26.0 darwin/arm64

# Linux
/usr/local/go/bin/go version
# expected: go version go1.26.0 linux/amd64
```

## 2. Install Python dependencies

Install `uv` if not already present:

**macOS:**
```bash
brew install uv
```

**Linux:**
```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

Then install project deps:
```bash
uv sync
```

This creates `.venv/` with all required packages. The scheduler uses `.venv/bin/python3` at runtime.

## 3. Configure the scheduler

```bash
cp scheduler/config.example.json scheduler/config.json
```

Edit `scheduler/config.json` and fill in your API keys.

## 4. Verify the setup

**macOS:**
```bash
# Compile check
cd scheduler && /opt/homebrew/bin/go build .

# Run unit tests
cd scheduler && /opt/homebrew/bin/go test ./...

# Smoke test (runs one cycle and exits)
./go-trader --once --config scheduler/config.json
```

**Linux:**
```bash
# Compile check
cd scheduler && /usr/local/go/bin/go build .

# Run unit tests
cd scheduler && /usr/local/go/bin/go test ./...

# Smoke test (runs one cycle and exits)
./go-trader --once --config scheduler/config.json
```

## 5. Deploy (Linux only)

Build the binary and restart the systemd service:
```bash
cd scheduler && /usr/local/go/bin/go build -o ../go-trader .
systemctl restart go-trader
```

For service file changes:
```bash
systemctl daemon-reload && systemctl restart go-trader
```
