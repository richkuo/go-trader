ARG GO_IMAGE=golang:1.26.2-bookworm
ARG PYTHON_IMAGE=python:3.12-slim-bookworm
ARG UV_IMAGE=ghcr.io/astral-sh/uv:0.9.5

FROM ${UV_IMAGE} AS uv

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG SOURCE_COMMIT=
WORKDIR /src/scheduler
COPY scheduler/go.mod scheduler/go.sum ./
RUN go mod download
COPY scheduler/ ./
RUN case "$SOURCE_COMMIT" in \
      "") ;; \
      *[!0-9a-f]*) echo "SOURCE_COMMIT must be a lowercase hex commit id, got: $SOURCE_COMMIT" >&2; exit 1 ;; \
      *) [ "${#SOURCE_COMMIT}" -eq 40 ] || [ "${#SOURCE_COMMIT}" -eq 64 ] || { echo "SOURCE_COMMIT must be a full commit id" >&2; exit 1; } ;; \
    esac \
 && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOFLAGS=-mod=readonly GOWORK=off \
    go build -buildvcs=false -trimpath \
      -ldflags "-X main.Version=${VERSION} -X main.SourceCommit=${SOURCE_COMMIT}" \
      -o /out/go-trader .

FROM ${PYTHON_IMAGE} AS python-deps
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1 \
    UV_PYTHON_DOWNLOADS=never \
    UV_PYTHON=/usr/local/bin/python3.12 \
    UV_PROJECT_ENVIRONMENT=/app/.venv
WORKDIR /app
COPY pyproject.toml uv.lock .python-version ./
RUN uv sync --frozen --no-dev --no-install-project \
 && /app/.venv/bin/python3 -c "import ccxt, hyperliquid, numpy, pandas, pyotp, robin_stocks, yfinance"

FROM ${PYTHON_IMAGE} AS runtime
ARG VERSION=dev
ARG SOURCE_COMMIT=
LABEL org.opencontainers.image.title="go-trader" \
      org.opencontainers.image.source="https://github.com/richkuo/go-trader" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"
RUN groupadd --system --gid 10001 gotrader \
 && useradd --system --uid 10001 --gid 10001 --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin gotrader \
 && mkdir -p /data /app/logs \
 && chown 10001:10001 /data /app/logs \
 && chmod 0700 /data \
 && chmod 0750 /app/logs
WORKDIR /app
COPY --from=python-deps /app/.venv /app/.venv
COPY shared_scripts/ /app/shared_scripts/
COPY shared_strategies/ /app/shared_strategies/
COPY shared_tools/ /app/shared_tools/
COPY platforms/ /app/platforms/
COPY backtest/ /app/backtest/
COPY pyproject.toml uv.lock LICENSE /app/
COPY --from=go-build /out/go-trader /app/go-trader
RUN /app/.venv/bin/python3 -m compileall -q /app/shared_scripts /app/shared_strategies /app/shared_tools /app/platforms /app/backtest \
 && chmod -R a-w,a+rX /app/.venv /app/shared_scripts /app/shared_strategies /app/shared_tools /app/platforms /app/backtest /app/pyproject.toml /app/uv.lock /app/LICENSE \
 && chmod 0555 /app/go-trader
ENV GO_TRADER_RUNTIME=container \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    MPLCONFIGDIR=/tmp/matplotlib \
    GO_TRADER_OHLCV_CACHE_DB=/data/ohlcv_cache.sqlite3
USER 10001:10001
EXPOSE 8099
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 CMD ["/app/go-trader", "healthcheck"]
ENTRYPOINT ["/app/go-trader"]
CMD ["supervise", "--config", "/data/config.json"]
