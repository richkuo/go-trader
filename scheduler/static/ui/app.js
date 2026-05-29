(function () {
  const SIDEBAR_STORAGE_KEY = "goTraderSidebarOpen";
  const MOBILE_SIDEBAR_MQ = "(max-width: 980px)";
  const VIEW_MODE_KEY = "goTraderViewMode";

  const state = {
    strategies: [],
    overviewRows: [],
    activeID: "",
    viewMode: "detail",
    sortKey: "id",
    sortDir: "asc",
    chart: null,
    series: null,
    timer: 0,
    sparklines: {},
  };

  const SPARKLINE_LIMIT = 40;

  const els = {
    count: document.getElementById("strategy-count"),
    list: document.getElementById("strategy-list"),
    search: document.getElementById("strategy-search"),
    title: document.getElementById("active-title"),
    regimeBadge: document.getElementById("regime-badge"),
    subtitle: document.getElementById("active-subtitle"),
    chart: document.getElementById("chart"),
    empty: document.getElementById("empty-chart"),
    chartWrap: document.querySelector(".chart-wrap"),
    tradeHistoryBody: document.getElementById("trade-history-body"),
    tradeHistoryEmpty: document.getElementById("trade-history-empty"),
    tradeHistoryTable: document.querySelector(".trade-history-table"),
    darkToggle: document.getElementById("dark-mode-toggle"),
    darkIcon: document.getElementById("dark-mode-icon"),
    refresh: document.getElementById("refresh-button"),
    viewMode: document.getElementById("view-mode-button"),
    interval: document.getElementById("refresh-interval"),
    statusDot: document.getElementById("status-dot"),
    statusLabel: document.getElementById("status-label"),
    authPanel: document.getElementById("auth-panel"),
    authToken: document.getElementById("auth-token"),
    statusGrid: document.getElementById("status-grid"),
    positions: document.getElementById("positions-list"),
    sidebar: document.getElementById("app-sidebar"),
    sidebarToggle: document.getElementById("sidebar-toggle"),
    sidebarBackdrop: document.getElementById("sidebar-backdrop"),
    workspace: document.querySelector(".workspace"),
    overviewPanel: document.getElementById("overview-panel"),
    overviewBody: document.getElementById("overview-body"),
    detailPanel: document.getElementById("detail-panel"),
  };

  function isMobileSidebar() {
    return window.matchMedia(MOBILE_SIDEBAR_MQ).matches;
  }

  function setSidebarOpen(open) {
    if (!isMobileSidebar()) {
      document.body.classList.remove("sidebar-open");
      if (els.workspace) {
        els.workspace.inert = false;
      }
      if (els.sidebarToggle) {
        els.sidebarToggle.setAttribute("aria-expanded", "false");
        els.sidebarToggle.setAttribute("aria-label", "Open menu");
      }
      if (els.sidebarBackdrop) {
        els.sidebarBackdrop.setAttribute("aria-hidden", "true");
      }
      try {
        sessionStorage.removeItem(SIDEBAR_STORAGE_KEY);
      } catch (_err) {
        /* sessionStorage unavailable */
      }
      return;
    }
    const wasOpen = document.body.classList.contains("sidebar-open");
    document.body.classList.toggle("sidebar-open", open);
    if (els.sidebarToggle) {
      els.sidebarToggle.setAttribute("aria-expanded", open ? "true" : "false");
      els.sidebarToggle.setAttribute("aria-label", open ? "Close menu" : "Open menu");
    }
    if (els.sidebarBackdrop) {
      els.sidebarBackdrop.setAttribute("aria-hidden", open ? "false" : "true");
    }
    if (els.workspace) {
      els.workspace.inert = open;
    }
    if (open && els.sidebar) {
      els.sidebar.focus();
    } else if (!open && wasOpen && els.sidebarToggle) {
      els.sidebarToggle.focus();
    }
    try {
      if (open) {
        sessionStorage.setItem(SIDEBAR_STORAGE_KEY, "1");
      } else {
        sessionStorage.removeItem(SIDEBAR_STORAGE_KEY);
      }
    } catch (_err) {
      /* sessionStorage unavailable */
    }
  }

  function readStoredSidebarOpen() {
    try {
      return sessionStorage.getItem(SIDEBAR_STORAGE_KEY) === "1";
    } catch (_err) {
      return false;
    }
  }

  function initSidebar() {
    if (!els.sidebarToggle || !els.sidebarBackdrop) return;

    function syncSidebarForViewport() {
      if (!isMobileSidebar()) {
        setSidebarOpen(false);
        return;
      }
      setSidebarOpen(readStoredSidebarOpen());
    }

    els.sidebarToggle.addEventListener("click", function () {
      setSidebarOpen(!document.body.classList.contains("sidebar-open"));
    });
    els.sidebarBackdrop.addEventListener("click", function () {
      setSidebarOpen(false);
    });
    window.matchMedia(MOBILE_SIDEBAR_MQ).addEventListener("change", syncSidebarForViewport);
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && document.body.classList.contains("sidebar-open")) {
        setSidebarOpen(false);
      }
    });
    syncSidebarForViewport();
  }

  function authHeaders() {
    const token = window.localStorage.getItem("goTraderStatusToken");
    return token ? { Authorization: "Bearer " + token } : {};
  }

  async function getJSON(url) {
    const res = await fetch(url, { headers: authHeaders() });
    if (!res.ok) {
      const text = await res.text();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      throw err;
    }
    return res.json();
  }

  function isDarkMode() {
    return document.documentElement.classList.contains("dark");
  }

  function chartThemeOptions() {
    const dark = isDarkMode();
    return {
      layout: {
        background: { type: "solid", color: dark ? "#1a211c" : "#ffffff" },
        textColor: dark ? "#c5cec8" : "#334139",
      },
      grid: {
        vertLines: { color: dark ? "#2b342f" : "#eef1eb" },
        horzLines: { color: dark ? "#2b342f" : "#eef1eb" },
      },
      rightPriceScale: { borderColor: dark ? "#3a4540" : "#d8ddd2" },
      timeScale: { borderColor: dark ? "#3a4540" : "#d8ddd2", timeVisible: true },
    };
  }

  function applyChartTheme() {
    if (!state.chart) return;
    state.chart.applyOptions(chartThemeOptions());
  }

  function updateDarkModeToggle() {
    const dark = isDarkMode();
    els.darkToggle.setAttribute("aria-pressed", dark ? "true" : "false");
    els.darkToggle.title = dark ? "Light mode" : "Dark mode";
    els.darkIcon.textContent = dark ? "☀" : "☾";
  }

  function setDarkMode(enabled) {
    document.documentElement.classList.toggle("dark", enabled);
    try {
      window.localStorage.setItem("goTraderDarkMode", enabled ? "1" : "0");
    } catch (e) {}
    updateDarkModeToggle();
    applyChartTheme();
  }


  function loadViewMode() {
    const saved = window.localStorage.getItem(VIEW_MODE_KEY);
    return saved === "table" ? "table" : "detail";
  }

  function saveViewMode(mode) {
    window.localStorage.setItem(VIEW_MODE_KEY, mode);
  }

  function applyViewMode() {
    const tableMode = state.viewMode === "table";
    els.overviewPanel.hidden = !tableMode;
    els.detailPanel.hidden = tableMode;
    els.viewMode.textContent = tableMode ? "Detail" : "Table";
    els.viewMode.setAttribute("aria-pressed", tableMode ? "true" : "false");
    document.querySelector(".content").classList.toggle("content-table", tableMode);
  }

  function toggleViewMode() {
    state.viewMode = state.viewMode === "table" ? "detail" : "table";
    saveViewMode(state.viewMode);
    applyViewMode();
    refreshAll().catch(handleRefreshError);
  }

  function initChart() {
    if (state.chart) return;
    state.chart = LightweightCharts.createChart(els.chart, Object.assign({}, chartThemeOptions(), {
      crosshair: { mode: LightweightCharts.CrosshairMode.Normal },
    }));
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

  function filteredStrategies() {
    const query = els.search.value.trim().toLowerCase();
    return state.strategies.filter(function (s) {
      const haystack = [s.id, s.platform, s.symbol, s.timeframe, s.strategy].join(" ").toLowerCase();
      return haystack.includes(query);
    });
  }

  function renderStrategies() {
    const filtered = filteredStrategies();
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
          '<span class="strategy-id"></span>' +
          '<canvas class="strategy-sparkline" width="48" height="28" aria-hidden="true"></canvas>' +
          '<span class="strategy-symbol"></span>' +
          '<span class="strategy-meta"></span>';
        button.querySelector(".strategy-id").textContent = strategy.id;
        button.querySelector(".strategy-symbol").textContent = strategy.symbol || "-";
        button.querySelector(".strategy-meta").textContent =
          [strategy.type, strategy.timeframe, strategy.direction].filter(Boolean).join(" / ");
        button.addEventListener("click", function () {
          selectStrategy(strategy.id).catch(handleRefreshError);
        });
        els.list.appendChild(button);
        const cached = state.sparklines[strategy.id];
        if (cached) {
          drawSparkline(button.querySelector(".strategy-sparkline"), cached);
        }
      });
    });
    loadSparklines(filtered.map(function (s) {
      return s.id;
    }));
  }

  function drawSparkline(canvas, points) {
    if (!canvas || !points || points.length < 2) {
      return;
    }
    const dpr = window.devicePixelRatio || 1;
    const cssW = canvas.clientWidth || 48;
    const cssH = canvas.clientHeight || 28;
    canvas.width = Math.round(cssW * dpr);
    canvas.height = Math.round(cssH * dpr);
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const values = points.map(function (p) {
      return Number(p.v);
    });
    const min = Math.min.apply(null, values);
    const max = Math.max.apply(null, values);
    const span = max - min || 1;
    const pad = 2;
    const up = values[values.length - 1] >= values[0];
    const color = up ? "#0f8a5f" : "#c23b3b";

    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.lineJoin = "round";
    ctx.lineCap = "round";
    ctx.beginPath();
    values.forEach(function (value, index) {
      const x = pad + (index / (values.length - 1)) * (cssW - pad * 2);
      const y = pad + (1 - (value - min) / span) * (cssH - pad * 2);
      if (index === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
  }

  async function loadSparklines(ids) {
    const unique = Array.from(new Set(ids));
    await Promise.all(unique.map(async function (id) {
      try {
        const resp = await getJSON(
          "/api/strategies/" + encodeURIComponent(id) + "/equity?limit=" + SPARKLINE_LIMIT
        );
        const points = resp.points || [];
        state.sparklines[id] = points;
        const button = els.list.querySelector('.strategy-button[data-id="' + CSS.escape(id) + '"]');
        if (button) {
          drawSparkline(button.querySelector(".strategy-sparkline"), points);
        }
      } catch (_err) {
        // Sidebar sparklines are best-effort; ignore per-strategy failures.
      }
    }));
  }

  function activeStrategy() {
    return state.strategies.find(function (s) {
      return s.id === state.activeID;
    });
  }

  async function selectStrategy(id, options) {
    const opts = options || {};
    state.activeID = id;
    updateRegimeBadge("");
    const strategy = activeStrategy();
    if (strategy) {
      els.title.textContent = strategy.id;
      els.subtitle.textContent = [strategy.platform, strategy.symbol, strategy.timeframe].filter(Boolean).join(" / ");
    }
    renderStrategies();
    if (opts.switchToDetail) {
      state.viewMode = "detail";
      saveViewMode(state.viewMode);
      applyViewMode();
    }
    if (isMobileSidebar()) {
      setSidebarOpen(false);
    }
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
    renderTradeHistory(tradeResp.trades || tradeResp.markers || []);
  }

  function tradeRowsForDisplay(rows) {
    const list = (rows || []).slice();
    list.reverse();
    return list;
  }

  function tradeSideLabel(trade) {
    if (trade.text) return trade.text;
    if (!trade.side) return "-";
    return trade.is_close ? "CLOSE" : String(trade.side).toUpperCase();
  }

  function tradeSideClass(trade) {
    const label = tradeSideLabel(trade);
    if (label === "BUY") return "trade-history-side-buy";
    if (label === "SELL") return "trade-history-side-sell";
    if (label === "CLOSE") return "";
    const side = trade.side ? String(trade.side).toLowerCase() : "";
    if (side === "buy") return "trade-history-side-buy";
    if (side === "sell") return "trade-history-side-sell";
    return "";
  }

  function tradePnLCell(trade) {
    if (!trade.is_close) return "-";
    if (trade.realized_pnl === undefined || trade.realized_pnl === null) return "-";
    return fmtSignedMoney(trade.realized_pnl);
  }

  function tradeRowClass(trade) {
    if (!trade.is_close || trade.realized_pnl === undefined || trade.realized_pnl === null) {
      return "";
    }
    if (trade.realized_pnl > 0) return "trade-history-row pnl-win";
    if (trade.realized_pnl < 0) return "trade-history-row pnl-loss";
    return "trade-history-row";
  }

  function fmtTradeTime(unixSeconds) {
    if (!unixSeconds) return "-";
    return new Date(unixSeconds * 1000).toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function renderTradeHistory(rows) {
    const trades = tradeRowsForDisplay(rows);
    if (!trades.length) {
      els.tradeHistoryBody.innerHTML = "";
      els.tradeHistoryEmpty.hidden = false;
      els.tradeHistoryTable.hidden = true;
      return;
    }
    els.tradeHistoryEmpty.hidden = true;
    els.tradeHistoryTable.hidden = false;
    els.tradeHistoryBody.innerHTML = trades.map(function (trade) {
      const sideClass = tradeSideClass(trade);
      const rowClass = tradeRowClass(trade);
      return (
        "<tr class=\"" + escapeHTML(rowClass) + "\">" +
        "<td>" + escapeHTML(fmtTradeTime(trade.time)) + "</td>" +
        "<td class=\"" + escapeHTML(sideClass) + "\">" + escapeHTML(tradeSideLabel(trade)) + "</td>" +
        "<td>" + escapeHTML(fmtMoney(trade.price)) + "</td>" +
        "<td>" + escapeHTML(fmtNumber(trade.quantity)) + "</td>" +
        "<td>" + escapeHTML(tradePnLCell(trade)) + "</td>" +
        "<td>" + escapeHTML(trade.regime || "-") + "</td>" +
        "<td>" + escapeHTML(trade.details || "-") + "</td>" +
        "</tr>"
      );
    }).join("");
  }

  function humanizeRegimeLabel(label) {
    return String(label).replace(/_/g, " ");
  }

  function regimeBadgeClass(label) {
    const key = String(label || "").toLowerCase();
    if (key.startsWith("trending_up") || key === "strong_trend_up" || key === "bull") {
      return "regime-badge--bull";
    }
    if (key.startsWith("trending_down") || key === "strong_trend_down" || key === "bear") {
      return "regime-badge--bear";
    }
    if (key.startsWith("ranging") || key === "weak_trend" || key === "neutral" || key === "default") {
      return "regime-badge--neutral";
    }
    return "regime-badge--unknown";
  }

  function updateRegimeBadge(regime) {
    const label = String(regime || "").trim();
    if (!label || label === "-") {
      els.regimeBadge.hidden = true;
      els.regimeBadge.textContent = "";
      els.regimeBadge.className = "regime-badge";
      return;
    }
    els.regimeBadge.className = "regime-badge " + regimeBadgeClass(label);
    els.regimeBadge.textContent = humanizeRegimeLabel(label);
    els.regimeBadge.hidden = false;
  }

  async function refreshStatus() {
    if (!state.activeID) return;
    const status = await getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/status");
    updateRegimeBadge(status.regime);
    els.statusDot.className = "status-dot ok";
    els.statusLabel.textContent = "Live";
    const drawdownPct = status.risk_state && status.risk_state.current_drawdown_pct;
    const fields = [
      ["Cash", fmtMoney(status.cash)],
      ["Initial", fmtMoney(status.initial_capital)],
      ["Value", fmtMoney(status.portfolio_value)],
      ["PnL", fmtSignedMoney(status.pnl), status.pnl],
      ["PnL %", fmtPct(status.pnl_pct), status.pnl_pct],
      ["Regime", status.regime || "-"],
      ["Drawdown", fmtPct(drawdownPct), drawdownPct, true],
      ["Leverage", fmtNumber(status.leverage)],
      ["Trades", String(status.lifetime_stats ? status.lifetime_stats.positions_opened || 0 : 0)],
      ["W/L", winLoss(status)],
      ["Win Rate", status.win_rate ? fmtPct(status.win_rate) : "-"],
      ["Sharpe", status.sharpe ? fmtNumber(status.sharpe) : "-"],
    ];
    els.statusGrid.innerHTML = fields.map(function (field) {
      const klass = field.length > 2 ? pnlClass(field[2], field[3]) : "";
      const dd = klass ? '<dd class="' + klass + '">' : "<dd>";
      return "<dt>" + escapeHTML(field[0]) + "</dt>" + dd + escapeHTML(field[1]) + "</dd>";
    }).join("");
    renderPositions(status.positions || {}, status.option_positions || {});
  }

  function winLoss(status) {
    const stats = status.lifetime_stats || {};
    const wins = stats.wins || 0;
    const losses = stats.losses || 0;
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


  function sortValue(row, key) {
    if (key === "pnl_pct" || key === "win_rate" || key === "sharpe") {
      const n = Number(row[key]);
      return Number.isFinite(n) ? n : -Infinity;
    }
    const value = row[key];
    return value === undefined || value === null ? "" : String(value).toLowerCase();
  }

  function sortedOverviewRows() {
    const rows = state.overviewRows.slice();
    const dir = state.sortDir === "desc" ? -1 : 1;
    rows.sort(function (a, b) {
      const av = sortValue(a, state.sortKey);
      const bv = sortValue(b, state.sortKey);
      if (av < bv) return -1 * dir;
      if (av > bv) return 1 * dir;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    });
    return rows;
  }

  function updateSortButtons() {
    document.querySelectorAll(".sort-button").forEach(function (button) {
      const key = button.dataset.key;
      const active = key === state.sortKey;
      button.classList.toggle("active", active);
      button.setAttribute("aria-sort", active ? (state.sortDir === "asc" ? "ascending" : "descending") : "none");
    });
  }

  function renderOverviewTable() {
    const rows = sortedOverviewRows();
    els.overviewBody.innerHTML = rows.map(function (row) {
      const pnlClassName = row.pnl_pct > 0 ? "pnl-pos" : row.pnl_pct < 0 ? "pnl-neg" : "";
      return '<tr class="overview-row' + (row.id === state.activeID ? " active" : "") + '" data-id="' + escapeHTML(row.id) + '">' +
        "<td>" + escapeHTML(row.id) + "</td>" +
        "<td>" + escapeHTML(row.platform || "-") + "</td>" +
        "<td>" + escapeHTML(row.symbol || "-") + "</td>" +
        '<td class="' + pnlClassName + '">' + escapeHTML(fmtPct(row.pnl_pct)) + "</td>" +
        "<td>" + escapeHTML(row.win_rate ? fmtPct(row.win_rate) : "-") + "</td>" +
        "<td>" + escapeHTML(row.sharpe ? fmtNumber(row.sharpe) : "-") + "</td>" +
        "<td>" + escapeHTML(row.regime || "-") + "</td>" +
        "<td>" + escapeHTML(row.direction || "-") + "</td>" +
        "</tr>";
    }).join("");
    updateSortButtons();
  }

  async function refreshOverview() {
    const resp = await getJSON("/api/strategies/overview");
    state.overviewRows = resp.strategies || [];
    renderOverviewTable();
    els.statusDot.className = "status-dot ok";
    els.statusLabel.textContent = "Live";
    els.statusGrid.innerHTML = "<dt>Strategies</dt><dd>" + escapeHTML(String(state.overviewRows.length)) + "</dd>";
    els.positions.innerHTML = '<div class="position-row"><span>Table view</span><span>Select a row for detail</span></div>';
  }

  function handleRefreshError(err) {
    if (err.status === 401) {
      showAuthPrompt();
      return;
    }
    els.statusDot.className = "status-dot error";
    els.statusLabel.textContent = "Error";
    els.statusGrid.innerHTML = "<dt>Message</dt><dd>" + escapeHTML(err.message) + "</dd>";
  }

  async function refreshAll() {
    try {
      if (state.viewMode === "table") {
        await refreshOverview();
        return;
      }
      await Promise.all([
        refreshChart(),
        refreshStatus(),
        loadSparklines(filteredStrategies().map(function (s) {
          return s.id;
        })),
      ]);
    } catch (err) {
      handleRefreshError(err);
    }
  }

  function scheduleRefresh() {
    if (state.timer) clearInterval(state.timer);
    const ms = Number(els.interval.value);
    if (ms > 0) {
      state.timer = setInterval(function () {
        refreshAll().catch(handleRefreshError);
      }, ms);
    }
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

  function pnlClass(value, invert) {
    const n = Number(value);
    if (value === undefined || value === null || Number.isNaN(n) || n === 0) return "";
    const positive = n > 0;
    if (invert) return positive ? "val-negative" : "val-positive";
    return positive ? "val-positive" : "val-negative";
  }

  function escapeHTML(value) {
    return String(value).replace(/[&<>"']/g, function (ch) {
      return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[ch];
    });
  }

  async function boot() {
    state.viewMode = loadViewMode();
    applyViewMode();
    updateDarkModeToggle();
    initChart();
    const resp = await getJSON("/api/strategies");
    state.strategies = resp.strategies || [];
    renderStrategies();
    if (state.strategies.length) {
      await selectStrategy(state.strategies[0].id);
    } else {
      await refreshAll();
    }
    scheduleRefresh();
  }

  initSidebar();
  els.search.addEventListener("input", renderStrategies);
  els.darkToggle.addEventListener("click", function () {
    setDarkMode(!isDarkMode());
  });
  els.refresh.addEventListener("click", function () {
    refreshAll().catch(handleRefreshError);
  });
  els.viewMode.addEventListener("click", toggleViewMode);
  els.interval.addEventListener("change", scheduleRefresh);
  els.overviewBody.addEventListener("click", function (event) {
    const row = event.target.closest(".overview-row");
    if (!row || !row.dataset.id) return;
    selectStrategy(row.dataset.id, { switchToDetail: true }).catch(handleRefreshError);
  });
  document.querySelectorAll(".sort-button").forEach(function (button) {
    button.addEventListener("click", function () {
      const key = button.dataset.key;
      if (state.sortKey === key) {
        state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
      } else {
        state.sortKey = key;
        state.sortDir = "asc";
      }
      renderOverviewTable();
    });
  });
  els.authPanel.addEventListener("submit", function (event) {
    event.preventDefault();
    const token = els.authToken.value.trim();
    if (token) {
      window.localStorage.setItem("goTraderStatusToken", token);
    } else {
      window.localStorage.removeItem("goTraderStatusToken");
    }
    els.authPanel.hidden = true;
    boot();
  });
  boot().catch(function (err) {
    if (err.status === 401) {
      showAuthPrompt();
      return;
    }
    handleRefreshError(err);
  });

  function showAuthPrompt() {
    els.statusDot.className = "status-dot error";
    els.statusLabel.textContent = "Token required";
    els.authToken.value = window.localStorage.getItem("goTraderStatusToken") || "";
    els.authPanel.hidden = false;
    els.statusGrid.innerHTML = "<dt>API</dt><dd>Unauthorized</dd>";
    els.authToken.focus();
  }
})();
