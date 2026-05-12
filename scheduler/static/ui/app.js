(function () {
  const state = {
    strategies: [],
    activeID: "",
    chart: null,
    series: null,
    timer: 0,
  };

  const els = {
    count: document.getElementById("strategy-count"),
    list: document.getElementById("strategy-list"),
    search: document.getElementById("strategy-search"),
    title: document.getElementById("active-title"),
    subtitle: document.getElementById("active-subtitle"),
    chart: document.getElementById("chart"),
    empty: document.getElementById("empty-chart"),
    refresh: document.getElementById("refresh-button"),
    interval: document.getElementById("refresh-interval"),
    statusDot: document.getElementById("status-dot"),
    statusLabel: document.getElementById("status-label"),
    statusGrid: document.getElementById("status-grid"),
    positions: document.getElementById("positions-list"),
  };

  function authHeaders() {
    const token = window.localStorage.getItem("goTraderStatusToken");
    return token ? { Authorization: "Bearer " + token } : {};
  }

  async function getJSON(url) {
    const res = await fetch(url, { headers: authHeaders() });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(text || res.statusText);
    }
    return res.json();
  }

  function initChart() {
    if (state.chart) return;
    state.chart = LightweightCharts.createChart(els.chart, {
      layout: {
        background: { type: "solid", color: "#ffffff" },
        textColor: "#334139",
      },
      grid: {
        vertLines: { color: "#eef1eb" },
        horzLines: { color: "#eef1eb" },
      },
      rightPriceScale: { borderColor: "#d8ddd2" },
      timeScale: { borderColor: "#d8ddd2", timeVisible: true },
      crosshair: { mode: LightweightCharts.CrosshairMode.Normal },
    });
    state.series = state.chart.addCandlestickSeries({
      upColor: "#0f8a5f",
      downColor: "#c23b3b",
      borderUpColor: "#0f8a5f",
      borderDownColor: "#c23b3b",
      wickUpColor: "#0f8a5f",
      wickDownColor: "#c23b3b",
    });
    new ResizeObserver(function () {
      const rect = els.chart.getBoundingClientRect();
      state.chart.resize(Math.max(320, rect.width), Math.max(320, rect.height));
    }).observe(els.chart);
  }

  function groupStrategies(strategies) {
    return strategies.reduce(function (groups, strategy) {
      const key = strategy.platform || "default";
      if (!groups[key]) groups[key] = [];
      groups[key].push(strategy);
      return groups;
    }, {});
  }

  function renderStrategies() {
    const query = els.search.value.trim().toLowerCase();
    const filtered = state.strategies.filter(function (s) {
      const haystack = [s.id, s.platform, s.symbol, s.timeframe, s.strategy].join(" ").toLowerCase();
      return haystack.includes(query);
    });
    els.count.textContent = filtered.length + " strategies";
    els.list.innerHTML = "";
    const groups = groupStrategies(filtered);
    Object.keys(groups).sort().forEach(function (platform) {
      const heading = document.createElement("div");
      heading.className = "platform-heading";
      heading.textContent = platform;
      els.list.appendChild(heading);
      groups[platform].forEach(function (strategy) {
        const button = document.createElement("button");
        button.className = "strategy-button" + (strategy.id === state.activeID ? " active" : "");
        button.type = "button";
        button.dataset.id = strategy.id;
        button.innerHTML =
          '<span class="strategy-id"></span><span class="strategy-symbol"></span><span class="strategy-meta"></span>';
        button.querySelector(".strategy-id").textContent = strategy.id;
        button.querySelector(".strategy-symbol").textContent = strategy.symbol || "-";
        button.querySelector(".strategy-meta").textContent =
          [strategy.type, strategy.timeframe, strategy.direction].filter(Boolean).join(" / ");
        button.addEventListener("click", function () {
          selectStrategy(strategy.id);
        });
        els.list.appendChild(button);
      });
    });
  }

  function activeStrategy() {
    return state.strategies.find(function (s) {
      return s.id === state.activeID;
    });
  }

  async function selectStrategy(id) {
    state.activeID = id;
    const strategy = activeStrategy();
    if (strategy) {
      els.title.textContent = strategy.id;
      els.subtitle.textContent = [strategy.platform, strategy.symbol, strategy.timeframe].filter(Boolean).join(" / ");
    }
    renderStrategies();
    await refreshAll();
  }

  function markerText(marker) {
    if (!marker.realized_pnl) return marker.text;
    const pnl = marker.realized_pnl >= 0 ? "+" + fmtMoney(marker.realized_pnl) : fmtMoney(marker.realized_pnl);
    return marker.text + " " + pnl;
  }

  async function refreshChart() {
    if (!state.activeID) return;
    initChart();
    const [candleResp, tradeResp] = await Promise.all([
      getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/candles?limit=400"),
      getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/trades?limit=400"),
    ]);
    const candles = candleResp.candles || [];
    state.series.setData(candles);
    state.series.setMarkers((tradeResp.markers || []).map(function (m) {
      return {
        time: m.time,
        position: m.position,
        color: m.color,
        shape: m.shape,
        text: markerText(m),
      };
    }));
    els.empty.style.display = candles.length ? "none" : "flex";
    if (candles.length) state.chart.timeScale().fitContent();
  }

  async function refreshStatus() {
    if (!state.activeID) return;
    const status = await getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/status");
    els.statusDot.className = "status-dot ok";
    els.statusLabel.textContent = "Live";
    const fields = [
      ["Cash", fmtMoney(status.cash)],
      ["Initial", fmtMoney(status.initial_capital)],
      ["Value", fmtMoney(status.portfolio_value)],
      ["PnL", fmtSignedMoney(status.pnl)],
      ["PnL %", fmtPct(status.pnl_pct)],
      ["Regime", status.regime || "-"],
      ["Drawdown", fmtPct(status.risk_state && status.risk_state.current_drawdown_pct)],
      ["Leverage", fmtNumber(status.leverage)],
      ["Trades", String(status.lifetime_stats ? status.lifetime_stats.PositionsOpened || status.lifetime_stats.positions_opened || 0 : 0)],
      ["W/L", winLoss(status)],
      ["Win Rate", status.win_rate ? fmtPct(status.win_rate) : "-"],
      ["Sharpe", status.sharpe ? fmtNumber(status.sharpe) : "-"],
    ];
    els.statusGrid.innerHTML = fields.map(function (field) {
      return "<dt>" + escapeHTML(field[0]) + "</dt><dd>" + escapeHTML(field[1]) + "</dd>";
    }).join("");
    renderPositions(status.positions || {}, status.option_positions || {});
  }

  function winLoss(status) {
    const stats = status.lifetime_stats || {};
    const wins = stats.Wins || stats.wins || 0;
    const losses = stats.Losses || stats.losses || 0;
    return wins || losses ? wins + "/" + losses : "-";
  }

  function renderPositions(positions, optionPositions) {
    const rows = [];
    Object.keys(positions).sort().forEach(function (symbol) {
      const pos = positions[symbol];
      rows.push(positionRow(symbol, pos.side || "long", pos.quantity, pos.avg_cost, pos.stop_loss_trigger_px));
    });
    Object.keys(optionPositions).sort().forEach(function (symbol) {
      const pos = optionPositions[symbol];
      rows.push(positionRow(symbol, pos.action || "", pos.quantity, pos.entry_premium_usd, 0));
    });
    els.positions.innerHTML = rows.length ? rows.join("") : '<div class="position-row"><span>Flat</span><span>-</span></div>';
  }

  function positionRow(symbol, side, qty, price, sl) {
    const klass = side === "short" || side === "sell" ? "pos-short" : "pos-long";
    const detail = "Qty " + fmtNumber(qty) + " @ " + fmtMoney(price) + (sl ? " / SL " + fmtMoney(sl) : "");
    return '<div class="position-row"><strong>' + escapeHTML(symbol) + '</strong><span class="' + klass + '">' +
      escapeHTML(side || "-") + '</span><span>' + escapeHTML(detail) + '</span><span></span></div>';
  }

  async function refreshAll() {
    try {
      await Promise.all([refreshChart(), refreshStatus()]);
    } catch (err) {
      els.statusDot.className = "status-dot error";
      els.statusLabel.textContent = "Error";
      els.statusGrid.innerHTML = "<dt>Message</dt><dd>" + escapeHTML(err.message) + "</dd>";
    }
  }

  function scheduleRefresh() {
    if (state.timer) clearInterval(state.timer);
    const ms = Number(els.interval.value);
    if (ms > 0) state.timer = setInterval(refreshStatus, ms);
  }

  function fmtMoney(value) {
    const n = Number(value || 0);
    return "$" + n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  }

  function fmtSignedMoney(value) {
    const n = Number(value || 0);
    return (n >= 0 ? "+" : "") + fmtMoney(n);
  }

  function fmtPct(value) {
    if (value === undefined || value === null || Number.isNaN(Number(value))) return "-";
    return Number(value).toFixed(2) + "%";
  }

  function fmtNumber(value) {
    if (value === undefined || value === null || Number.isNaN(Number(value))) return "-";
    return Number(value).toLocaleString(undefined, { maximumFractionDigits: 4 });
  }

  function escapeHTML(value) {
    return String(value).replace(/[&<>"']/g, function (ch) {
      return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[ch];
    });
  }

  async function boot() {
    initChart();
    const resp = await getJSON("/api/strategies");
    state.strategies = resp.strategies || [];
    renderStrategies();
    if (state.strategies.length) {
      await selectStrategy(state.strategies[0].id);
    }
    scheduleRefresh();
  }

  els.search.addEventListener("input", renderStrategies);
  els.refresh.addEventListener("click", refreshAll);
  els.interval.addEventListener("change", scheduleRefresh);
  boot().catch(function (err) {
    els.statusDot.className = "status-dot error";
    els.statusLabel.textContent = "Error";
    els.statusGrid.innerHTML = "<dt>Message</dt><dd>" + escapeHTML(err.message) + "</dd>";
  });
})();
