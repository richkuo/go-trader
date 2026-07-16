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
    tuner: {
      config: null,
      overrides: {},
      liveMarkers: [],
      simulatedMarkers: [],
      previewActive: false,
      simulateTimer: 0,
      loading: false,
    },
  };

  const SPARKLINE_LIMIT = 40;

  const els = {
    count: document.getElementById("strategy-count"),
    list: document.getElementById("strategy-list"),
    search: document.getElementById("strategy-search"),
    title: document.getElementById("active-title"),
    regimeBadge: document.getElementById("regime-badge"),
    pausedBadge: document.getElementById("paused-badge"),
    riskContent: document.getElementById("risk-content"),
    regimeStoreContent: document.getElementById("regime-store-content"),
    transitionsContent: document.getElementById("transitions-content"),
    divergenceBadge: document.getElementById("divergence-badge"),
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
    tunerPanel: document.getElementById("tuner-panel"),
    tunerForm: document.getElementById("tuner-form"),
    tunerStatus: document.getElementById("tuner-status"),
    tunerReset: document.getElementById("tuner-reset"),
    tunerApply: document.getElementById("tuner-apply"),
    pauseToggle: document.getElementById("pause-toggle"),
    ratchetNotifySelect: document.getElementById("ratchet-notify-select"),
    globalRatchetSelect: document.getElementById("global-ratchet-select"),
    controlsMessage: document.getElementById("controls-message"),
    tunerConfirmDialog: document.getElementById("tuner-confirm-dialog"),
    tunerConfirmText: document.getElementById("tuner-confirm-text"),
    tradePanel: document.getElementById("trade-panel"),
    tradeOpenForm: document.getElementById("trade-open-form"),
    tradeOpenSide: document.getElementById("trade-open-side"),
    tradeSizingMode: document.getElementById("trade-sizing-mode"),
    tradeSizingAmount: document.getElementById("trade-sizing-amount"),
    tradeOpenButton: document.getElementById("trade-open-button"),
    tradeAddButton: document.getElementById("trade-add-button"),
    tradePositionForm: document.getElementById("trade-position-form"),
    tradeCloseQty: document.getElementById("trade-close-qty"),
    tradeSLTrigger: document.getElementById("trade-sl-trigger"),
    tradeMessage: document.getElementById("trade-message"),
    tradeConfirmDialog: document.getElementById("trade-confirm-dialog"),
    tradeConfirmDesc: document.getElementById("trade-confirm-desc"),
    tradeConfirmPhrase: document.getElementById("trade-confirm-phrase"),
    tradeConfirmTTL: document.getElementById("trade-confirm-ttl"),
    tradeConfirmInput: document.getElementById("trade-confirm-input"),
    tradeConfirmGo: document.getElementById("trade-confirm-go"),
    structuralPanel: document.getElementById("structural-panel"),
    structuralRestart: document.getElementById("structural-restart"),
    structuralMessage: document.getElementById("structural-message"),
    paperToLiveButton: document.getElementById("paper-to-live-button"),
    applyRegimeGateButton: document.getElementById("apply-regime-gate-button"),
    removeStrategyButton: document.getElementById("remove-strategy-button"),
    addStratName: document.getElementById("add-strat-name"),
    addStratPlatform: document.getElementById("add-strat-platform"),
    addStratAsset: document.getElementById("add-strat-asset"),
    addStratRestart: document.getElementById("add-strat-restart"),
    addStratButton: document.getElementById("add-strat-button"),
    addStratMessage: document.getElementById("add-strat-message"),
    leaderboardBody: document.getElementById("leaderboard-body"),
    leaderboardEmpty: document.getElementById("leaderboard-empty"),
    diagnosticsBody: document.getElementById("diagnostics-body"),
    diagnosticsEmpty: document.getElementById("diagnostics-empty"),
    cashflowContent: document.getElementById("cashflow-content"),
    correlationContent: document.getElementById("correlation-content"),
    deadStrategiesContent: document.getElementById("dead-strategies-content"),
    closingStrategiesContent: document.getElementById("closing-strategies-content"),
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

  async function postJSON(url, body) {
    const res = await fetch(url, {
      method: "POST",
      headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
      body: JSON.stringify(body || {}),
    });
    if (!res.ok) {
      const text = await res.text();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      throw err;
    }
    return res.json();
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
        button.querySelector(".strategy-id").textContent = (strategy.paused ? "⏸ " : "") + strategy.id;
        if (strategy.paused) {
          button.title = "Paused — position-increasing signals held";
        }
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
    resetTunerState();
    updateRegimeBadge("");
    updateDivergenceBadge(null);
    updatePausedBadge(false);
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

  function resetTunerState() {
    state.tuner.config = null;
    state.tuner.overrides = {};
    state.tuner.liveMarkers = [];
    state.tuner.simulatedMarkers = [];
    state.tuner.previewActive = false;
    if (state.tuner.simulateTimer) {
      clearTimeout(state.tuner.simulateTimer);
      state.tuner.simulateTimer = 0;
    }
    updateTunerStatus();
    if (els.tunerPanel) {
      els.tunerPanel.hidden = true;
    }
    if (els.tunerForm) {
      els.tunerForm.innerHTML = "";
    }
  }

  function tunerHasOverrides() {
    return Object.keys(state.tuner.overrides).length > 0;
  }

  function updateTunerStatus() {
    if (!els.tunerStatus) return;
    if (state.tuner.loading) {
      els.tunerStatus.textContent = "Simulating…";
      els.tunerStatus.className = "tuner-status preview";
      return;
    }
    if (tunerHasOverrides()) {
      els.tunerStatus.textContent = "Preview active";
      els.tunerStatus.className = "tuner-status preview";
      return;
    }
    els.tunerStatus.textContent = "Live config";
    els.tunerStatus.className = "tuner-status";
  }

  function groupEditableFields(fields) {
    const groups = { runtime: [], risk: [], open_params: [] };
    (fields || []).forEach(function (field) {
      const key = field.group || "open_params";
      if (!groups[key]) groups[key] = [];
      groups[key].push(field);
    });
    return groups;
  }

  function renderTunerForm(config) {
    if (!els.tunerForm || !els.tunerPanel) return;
    if (!config || !config.editable_fields || !config.editable_fields.length) {
      els.tunerPanel.hidden = true;
      els.tunerForm.innerHTML = "";
      return;
    }
    els.tunerPanel.hidden = false;
    const groups = groupEditableFields(config.editable_fields);
    const groupLabels = {
      runtime: "Runtime",
      risk: "Risk",
      open_params: "Indicator params",
    };
    let html = "";
    ["runtime", "risk", "open_params"].forEach(function (groupKey) {
      const fields = groups[groupKey] || [];
      if (!fields.length) return;
      html += '<div class="tuner-group-label">' + escapeHTML(groupLabels[groupKey] || groupKey) + "</div>";
      fields.forEach(function (field) {
        const value = state.tuner.overrides[field.key] !== undefined
          ? state.tuner.overrides[field.key]
          : field.value;
        html += '<label class="tuner-field" data-key="' + escapeHTML(field.key) + '">';
        html += "<span>" + escapeHTML(field.label || field.key) + "</span>";
        if (field.type === "boolean") {
          html += '<input type="checkbox" data-key="' + escapeHTML(field.key) + '"' +
            (value ? " checked" : "") + ">";
        } else if (field.type === "select") {
          html += '<select data-key="' + escapeHTML(field.key) + '">';
          (field.options || []).forEach(function (opt) {
            html += '<option value="' + escapeHTML(opt) + '"' +
              (String(value) === String(opt) ? " selected" : "") + ">" +
              escapeHTML(opt) + "</option>";
          });
          html += "</select>";
        } else {
          html += '<input type="number" step="any" data-key="' + escapeHTML(field.key) + '" value="' +
            escapeHTML(value === null || value === undefined ? "" : value) + '">';
        }
        html += "</label>";
      });
    });
    els.tunerForm.innerHTML = html;
  }

  async function loadTunerConfig() {
    if (!state.activeID) return;
    const config = await getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/config");
    state.tuner.config = config;
    renderTunerForm(config);
    updateControlsPanel(config);
    refreshGlobalNotifications().catch(function () {});
  }

  // #1256 low-risk mutation controls: pause/unpause + ratchet-alert toggles.
  function setControlsMessage(text) {
    if (!els.controlsMessage) return;
    els.controlsMessage.textContent = text || "";
    els.controlsMessage.hidden = !text;
  }

  function updateControlsPanel(config) {
    if (els.pauseToggle) {
      els.pauseToggle.hidden = !config;
      if (config) {
        els.pauseToggle.textContent = config.paused ? "Resume strategy" : "Pause strategy";
        els.pauseToggle.dataset.paused = config.paused ? "1" : "";
      }
    }
    if (els.ratchetNotifySelect && config) {
      const v = config.notify_ratchet_triggers;
      els.ratchetNotifySelect.value = v === null || v === undefined ? "inherit" : (v ? "on" : "off");
    }
  }

  async function refreshGlobalNotifications() {
    if (!els.globalRatchetSelect) return;
    const resp = await getJSON("/api/config/notifications");
    const v = resp.notify_ratchet_triggers;
    els.globalRatchetSelect.value = v === null || v === undefined ? "default" : (v ? "on" : "off");
  }

  function triStateToValue(v, inheritKey) {
    if (v === inheritKey) return null;
    return v === "on";
  }

  async function togglePause() {
    if (!state.activeID || !els.pauseToggle) return;
    const next = !els.pauseToggle.dataset.paused;
    els.pauseToggle.disabled = true;
    try {
      const resp = await postJSON(
        "/api/strategies/" + encodeURIComponent(state.activeID) + "/pause",
        { paused: next }
      );
      setControlsMessage(resp.message || "");
      await loadTunerConfig();
      await refreshAll();
    } catch (err) {
      setControlsMessage("Pause failed: " + err.message);
    } finally {
      els.pauseToggle.disabled = false;
    }
  }

  async function changeStrategyRatchetNotify() {
    if (!state.activeID || !els.ratchetNotifySelect) return;
    try {
      const resp = await postJSON(
        "/api/strategies/" + encodeURIComponent(state.activeID) + "/notifications",
        { notify_ratchet_triggers: triStateToValue(els.ratchetNotifySelect.value, "inherit") }
      );
      setControlsMessage(resp.message || "");
    } catch (err) {
      setControlsMessage("Notification toggle failed: " + err.message);
    }
  }

  async function changeGlobalRatchetNotify() {
    if (!els.globalRatchetSelect) return;
    try {
      const resp = await postJSON("/api/config/notifications", {
        notify_ratchet_triggers: triStateToValue(els.globalRatchetSelect.value, "default"),
      });
      setControlsMessage(resp.message || "");
    } catch (err) {
      setControlsMessage("Global notification toggle failed: " + err.message);
    }
  }

  // #1257 trade actions: server-issued confirm nonce + typed confirmation.
  function setTradeMessage(text) {
    if (!els.tradeMessage) return;
    els.tradeMessage.textContent = text || "";
    els.tradeMessage.hidden = !text;
  }

  function activeStrategyMeta() {
    return state.strategies.find(function (s) { return s.id === state.activeID; }) || null;
  }

  function strategySupportsManualActions(strat) {
    return !!strat && strat.type === "manual";
  }

  function strategySupportsForceClose(strat) {
    return !!strat && strat.type === "perps" && strat.platform === "hyperliquid";
  }

  // updateTradePanel shows the action surface matching the active strategy:
  // manual-open form for flat manual strategies, add/close/SL forms while a
  // position is open, force-close for HL perps.
  function updateTradePanel(status) {
    if (!els.tradePanel) return;
    const strat = activeStrategyMeta();
    const manual = strategySupportsManualActions(strat);
    const forceClose = strategySupportsForceClose(strat);
    const hasPosition = !!(status && status.positions && Object.keys(status.positions).length);
    els.tradePanel.hidden = !(manual || forceClose);
    if (els.tradeOpenForm) {
      els.tradeOpenForm.hidden = !manual;
    }
    if (els.tradeOpenButton) {
      els.tradeOpenButton.hidden = !manual || hasPosition;
    }
    if (els.tradeAddButton) {
      els.tradeAddButton.hidden = !manual || !hasPosition;
    }
    if (els.tradePositionForm) {
      els.tradePositionForm.hidden = !hasPosition;
    }
    const slField = document.getElementById("trade-sl-field");
    if (slField) {
      slField.hidden = !manual;
    }
  }

  // confirmTradeAction runs the full #1257 flow: POST /api/confirm for a
  // nonce bound to (action, strategy, params), require the operator to type
  // the server-issued phrase, then POST the action with the nonce. The
  // response message is the queued outcome reported by the manual core.
  async function confirmTradeAction(action, params) {
    if (!state.activeID) return;
    setTradeMessage("");
    let confirm;
    try {
      confirm = await postJSON("/api/confirm", {
        action: action,
        strategy_id: state.activeID,
        params: params,
      });
    } catch (err) {
      setTradeMessage("Confirm failed: " + err.message);
      return;
    }
    const proceed = await showTradeConfirmDialog(confirm);
    if (!proceed) {
      setTradeMessage("Cancelled.");
      return;
    }
    try {
      const resp = await postJSON(
        "/api/strategies/" + encodeURIComponent(state.activeID) + "/" + action,
        { nonce: confirm.nonce, params: params }
      );
      setTradeMessage(resp.message || "Submitted.");
      await refreshAll();
    } catch (err) {
      setTradeMessage(action + " failed: " + err.message);
    }
  }

  // showTradeConfirmDialog resolves true only when the operator typed the
  // exact confirm phrase and pressed Confirm.
  function showTradeConfirmDialog(confirm) {
    return new Promise(function (resolve) {
      if (!els.tradeConfirmDialog || typeof els.tradeConfirmDialog.showModal !== "function") {
        const typed = window.prompt((confirm.description || "Confirm action") +
          '\nType "' + confirm.confirm_phrase + '" to confirm:');
        resolve(typed === confirm.confirm_phrase);
        return;
      }
      els.tradeConfirmDesc.textContent = confirm.description || "";
      els.tradeConfirmPhrase.textContent = confirm.confirm_phrase || "";
      if (els.tradeConfirmTTL) {
        els.tradeConfirmTTL.textContent = String(confirm.expires_in_seconds || 60);
      }
      els.tradeConfirmInput.value = "";
      els.tradeConfirmGo.disabled = true;
      const onInput = function () {
        els.tradeConfirmGo.disabled = els.tradeConfirmInput.value !== confirm.confirm_phrase;
      };
      const onClose = function () {
        els.tradeConfirmInput.removeEventListener("input", onInput);
        els.tradeConfirmDialog.removeEventListener("close", onClose);
        resolve(els.tradeConfirmDialog.returnValue === "confirm" &&
          els.tradeConfirmInput.value === confirm.confirm_phrase);
      };
      els.tradeConfirmInput.addEventListener("input", onInput);
      els.tradeConfirmDialog.addEventListener("close", onClose);
      els.tradeConfirmDialog.showModal();
      els.tradeConfirmInput.focus();
    });
  }

  function tradeSizingParams() {
    const params = {};
    const amount = Number(els.tradeSizingAmount && els.tradeSizingAmount.value);
    if (amount > 0 && els.tradeSizingMode) {
      params[els.tradeSizingMode.value] = amount;
    }
    return params;
  }

  function tradeOpen() {
    const params = tradeSizingParams();
    if (els.tradeOpenSide && els.tradeOpenSide.value) {
      params.side = els.tradeOpenSide.value;
    }
    return confirmTradeAction("open", params);
  }

  function tradeAdd() {
    return confirmTradeAction("add", tradeSizingParams());
  }

  function tradeClose(action) {
    const params = {};
    const qty = Number(els.tradeCloseQty && els.tradeCloseQty.value);
    if (qty > 0) {
      params.qty = qty;
    }
    return confirmTradeAction(action, params);
  }

  function tradeUpdateSL() {
    const trigger = Number(els.tradeSLTrigger && els.tradeSLTrigger.value);
    if (!(trigger > 0)) {
      setTradeMessage("Enter a stop-loss trigger price first.");
      return Promise.resolve();
    }
    return confirmTradeAction("update-sl", { trigger: trigger });
  }

  function tradeCancelSL() {
    return confirmTradeAction("cancel-sl", {});
  }

  // #1258 structural mutations: same confirm-nonce + typed-confirmation flow
  // as trade actions, but the endpoints and target routing differ (add-strategy
  // has no existing target). All four are restart-required config writes; the
  // server restarts only when params.restart is confirmed true.
  function setStructuralMessage(text, el) {
    const target = el || els.structuralMessage;
    if (!target) return;
    target.textContent = text || "";
    target.hidden = !text;
  }

  // updateStructuralPanel shows the structural surface for the active
  // strategy: remove for all, paper→live + regime gate for perps/futures
  // (server re-validates eligibility either way).
  function updateStructuralPanel() {
    if (!els.structuralPanel) return;
    const strat = activeStrategyMeta();
    els.structuralPanel.hidden = !strat;
    if (!strat) return;
    const gateable = strat.type === "perps" || strat.type === "futures";
    if (els.paperToLiveButton) els.paperToLiveButton.hidden = !gateable;
    if (els.applyRegimeGateButton) els.applyRegimeGateButton.hidden = !gateable;
  }

  // confirmStructuralAction mirrors confirmTradeAction with an explicit
  // endpoint URL and strategy id ("" for add-strategy).
  async function confirmStructuralAction(action, strategyID, params, url, messageEl) {
    setStructuralMessage("", messageEl);
    let confirm;
    try {
      confirm = await postJSON("/api/confirm", {
        action: action,
        strategy_id: strategyID,
        params: params,
      });
    } catch (err) {
      setStructuralMessage("Confirm failed: " + err.message, messageEl);
      return;
    }
    const proceed = await showTradeConfirmDialog(confirm);
    if (!proceed) {
      setStructuralMessage("Cancelled.", messageEl);
      return;
    }
    try {
      const resp = await postJSON(url, { nonce: confirm.nonce, params: params });
      setStructuralMessage(resp.message || "Done.", messageEl);
      await refreshAll();
    } catch (err) {
      setStructuralMessage(action + " failed: " + err.message, messageEl);
    }
  }

  function structuralParamsBase() {
    const params = {};
    if (els.structuralRestart && els.structuralRestart.checked) {
      params.restart = true;
    }
    return params;
  }

  function structuralAddStrategy() {
    const name = (els.addStratName && els.addStratName.value || "").trim();
    const platform = els.addStratPlatform ? els.addStratPlatform.value : "";
    const asset = (els.addStratAsset && els.addStratAsset.value || "").trim();
    if (!name || !asset) {
      setStructuralMessage("Strategy name and asset are required.", els.addStratMessage);
      return Promise.resolve();
    }
    const params = { name: name, platform: platform, asset: asset };
    if (els.addStratRestart && els.addStratRestart.checked) {
      params.restart = true;
    }
    return confirmStructuralAction("add-strategy", "", params, "/api/config/add-strategy", els.addStratMessage);
  }

  function structuralPerStrategy(action) {
    if (!state.activeID) return Promise.resolve();
    return confirmStructuralAction(
      action,
      state.activeID,
      structuralParamsBase(),
      "/api/strategies/" + encodeURIComponent(state.activeID) + "/" + action,
      els.structuralMessage
    );
  }

  function buildSimulateOverrides() {
    const overrides = {};
    Object.keys(state.tuner.overrides).forEach(function (key) {
      overrides[key] = state.tuner.overrides[key];
    });
    if (state.tuner.config && state.tuner.config.open_strategy && state.tuner.config.open_strategy.params) {
      const paramOverrides = {};
      Object.keys(overrides).forEach(function (key) {
        if (key.indexOf("open_strategy.params.") === 0) {
          paramOverrides[key.slice("open_strategy.params.".length)] = overrides[key];
          delete overrides[key];
        }
      });
      if (Object.keys(paramOverrides).length) {
        overrides.open_strategy = {
          name: state.tuner.config.open_strategy.name,
          params: paramOverrides,
        };
      }
    }
    return overrides;
  }

  function scheduleSimulate() {
    if (state.tuner.simulateTimer) {
      clearTimeout(state.tuner.simulateTimer);
    }
    state.tuner.simulateTimer = setTimeout(function () {
      runSimulate().catch(handleRefreshError);
    }, 450);
  }

  async function runSimulate() {
    if (!state.activeID) return;
    if (!tunerHasOverrides()) {
      state.tuner.previewActive = false;
      state.tuner.liveMarkers = [];
      state.tuner.simulatedMarkers = [];
      updateTunerStatus();
      await refreshChart();
      return;
    }
    state.tuner.loading = true;
    updateTunerStatus();
    try {
      const resp = await postJSON(
        "/api/strategies/" + encodeURIComponent(state.activeID) + "/simulate",
        { overrides: buildSimulateOverrides(), limit: 400 }
      );
      state.tuner.liveMarkers = resp.live_markers || [];
      state.tuner.simulatedMarkers = resp.simulated_markers || [];
      state.tuner.previewActive = true;
      await refreshChart();
    } finally {
      state.tuner.loading = false;
      updateTunerStatus();
    }
  }

  function onTunerFieldChange(event) {
    const target = event.target;
    const key = target.dataset.key;
    if (!key) return;
    let value;
    if (target.type === "checkbox") {
      value = target.checked;
    } else if (target.type === "number") {
      value = target.value === "" ? null : Number(target.value);
    } else {
      value = target.value;
    }
    const base = state.tuner.config && state.tuner.config.editable_fields
      ? state.tuner.config.editable_fields.find(function (f) { return f.key === key; })
      : null;
    const baseline = base ? base.value : undefined;
    if (value === baseline || (value === null && (baseline === null || baseline === undefined))) {
      delete state.tuner.overrides[key];
    } else {
      state.tuner.overrides[key] = value;
    }
    updateTunerStatus();
    scheduleSimulate();
  }

  function resetTunerToLive() {
    state.tuner.overrides = {};
    if (state.tuner.config) {
      renderTunerForm(state.tuner.config);
    }
    state.tuner.previewActive = false;
    state.tuner.liveMarkers = [];
    state.tuner.simulatedMarkers = [];
    updateTunerStatus();
    refreshChart().catch(handleRefreshError);
  }

  function tunerApplyRequiresRestartClient() {
    const keys = Object.keys(state.tuner.overrides);
    if (state.tuner.config && state.tuner.config.has_open_position) {
      if (keys.indexOf("direction") >= 0 || keys.indexOf("invert_signal") >= 0 || keys.indexOf("leverage") >= 0) {
        return true;
      }
    }
    return keys.indexOf("htf_filter") >= 0;
  }

  async function applyTunerConfig() {
    if (!state.activeID || !tunerHasOverrides()) return;
    const resp = await postJSON(
      "/api/strategies/" + encodeURIComponent(state.activeID) + "/config",
      { overrides: buildSimulateOverrides() }
    );
    if (resp.ok) {
      resetTunerState();
      await loadTunerConfig();
      await refreshAll();
      els.statusLabel.textContent = resp.restart_required ? "Apply OK — restart" : "Apply OK";
    }
  }

  function markerText(marker) {
    if (!marker.realized_pnl) return marker.text;
    const pnl = marker.realized_pnl >= 0 ? "+" + fmtMoney(marker.realized_pnl) : fmtMoney(marker.realized_pnl);
    return marker.text + " " + pnl;
  }

  function chartMarkersFromResponse(tradeResp) {
    if (state.tuner.previewActive && state.tuner.simulatedMarkers.length) {
      const faded = (state.tuner.liveMarkers || []).map(function (m) {
        return {
          time: m.time,
          position: m.position,
          color: "#9ca3af",
          shape: m.shape,
          text: markerText(m),
        };
      });
      const bright = state.tuner.simulatedMarkers.map(function (m) {
        return {
          time: m.time,
          position: m.position,
          color: m.color,
          shape: m.shape,
          text: markerText(m),
        };
      });
      return faded.concat(bright);
    }
    return (tradeResp.markers || []).map(function (m) {
      return {
        time: m.time,
        position: m.position,
        color: m.color,
        shape: m.shape,
        text: markerText(m),
      };
    });
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
    state.series.setMarkers(chartMarkersFromResponse(tradeResp));
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

  function updateDivergenceBadge(divergence) {
    if (!divergence || divergence.kind !== "hard" || !divergence.resolved_direction) {
      els.divergenceBadge.hidden = true;
      els.divergenceBadge.textContent = "";
      return;
    }
    const dir = divergence.resolved_direction === "long" ? "↑" : "↓";
    els.divergenceBadge.textContent = "⚠ divergence " + dir + " (" + divergence.cycles_active + "c)";
    els.divergenceBadge.title = "Regime divergence: short=" + divergence.short + " medium=" + divergence.medium +
      " → " + divergence.resolved_direction + " (" + divergence.cycles_active + " cycles)";
    els.divergenceBadge.hidden = false;
  }

  function updatePausedBadge(paused) {
    if (!els.pausedBadge) return;
    els.pausedBadge.hidden = !paused;
  }

  function directionCell(status) {
    if (!status.regime_directional_policy) {
      return null;
    }
    const dir = status.effective_direction || "-";
    const cert = status.directional_certification_status || "";
    // #1085/#1157: uncertified/expired cells run DEFAULT-OFF (BASE direction).
    if (cert && cert !== "certified") {
      return dir + " (" + cert.toUpperCase() + " → default-off)";
    }
    return dir + (cert ? " (certified)" : "");
  }

  async function refreshStatus() {
    if (!state.activeID) return;
    const status = await getJSON("/api/strategies/" + encodeURIComponent(state.activeID) + "/status");
    updateRegimeBadge(status.regime);
    updateDivergenceBadge(status.regime_divergence);
    updatePausedBadge(!!status.paused);
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
    const dirCell = directionCell(status);
    if (dirCell) {
      fields.push(["Direction", dirCell]);
    }
    if (status.regime_profile && status.regime_profile.active_profile) {
      let profile = status.regime_profile.active_profile;
      if (status.regime_profile.pending_profile) {
        profile += " → " + status.regime_profile.pending_profile + " pending";
      }
      fields.push(["Profile", profile]);
    }
    if (status.hedge && status.hedge.enabled) {
      const ratio = status.hedge.ratio || 1;
      fields.push(["Hedge", "Inverse " + (status.hedge.symbol || "-") + " × " + fmtNumber(ratio)]);
    }
    els.statusGrid.innerHTML = fields.map(function (field) {
      const klass = field.length > 2 ? pnlClass(field[2], field[3]) : "";
      const dd = klass ? '<dd class="' + klass + '">' : "<dd>";
      return "<dt>" + escapeHTML(field[0]) + "</dt>" + dd + escapeHTML(field[1]) + "</dd>";
    }).join("");
    renderPositions(status.positions || {}, status.option_positions || {});
    updateTradePanel(status);
    updateStructuralPanel();
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
      rows.push(positionRow(symbol, pos.side || "long", pos.quantity, pos.avg_cost, pos.stop_loss_trigger_px, pos));
    });
    Object.keys(optionPositions).sort().forEach(function (symbol) {
      const pos = optionPositions[symbol];
      rows.push(positionRow(symbol, pos.action || "", pos.quantity, pos.entry_premium_usd, 0, null));
    });
    els.positions.innerHTML = rows.length ? rows.join("") : '<div class="position-row"><span>Flat</span><span>-</span></div>';
  }

  function positionRow(symbol, side, qty, price, sl, pos) {
    const klass = side === "short" || side === "sell" ? "pos-short" : "pos-long";
    let detail = "Qty " + fmtNumber(qty) + " @ " + fmtMoney(price) + (sl ? " / SL " + fmtMoney(sl) : "");
    if (pos && pos.hedge_for) {
      detail += " / Hedge for " + pos.hedge_for + " (primary basis " + fmtNumber(pos.hedge_primary_qty_basis || 0) + ")";
    }
    return '<div class="position-row"><strong>' + escapeHTML(symbol) + '</strong><span class="' + klass + '">' +
      escapeHTML(side || "-") + '</span><span>' + escapeHTML(detail) + '</span><span>' + positionActionButtons() + '</span></div>';
  }

  // positionActionButtons renders the #1257 per-row actions for eligible
  // strategies; every button funnels through the confirm-nonce dialog.
  function positionActionButtons() {
    const strat = activeStrategyMeta();
    const buttons = [];
    if (strategySupportsManualActions(strat)) {
      buttons.push('<button type="button" class="trade-row-button" data-trade-action="close">Close</button>');
      buttons.push('<button type="button" class="trade-row-button" data-trade-action="update-sl">Edit SL</button>');
      buttons.push('<button type="button" class="trade-row-button" data-trade-action="cancel-sl">Cancel SL</button>');
    } else if (strategySupportsForceClose(strat)) {
      buttons.push('<button type="button" class="trade-row-button danger" data-trade-action="force-close">Force close</button>');
    }
    return buttons.join("");
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
        "<td>" + (row.paused ? '<span title="Paused">⏸</span> ' : "") + escapeHTML(row.id) + "</td>" +
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

  // ── Risk / regime-store / transitions panels (#1230) ─────────────────────
  // All three are best-effort: a failing source renders a "-" placeholder and
  // never breaks the rest of the page (#879 fail-open convention).

  function panelFallback(el, text) {
    if (el) {
      el.innerHTML = '<div class="panel-row panel-muted">' + escapeHTML(text) + "</div>";
    }
  }

  function cbUntilLabel(untilISO, now) {
    if (!untilISO) return "no expiry set";
    const until = new Date(untilISO).getTime();
    if (Number.isNaN(until) || until <= 0 || untilISO.indexOf("0001-") === 0) return "no expiry set";
    if (until <= now) return "expired (clears next cycle)";
    const mins = Math.round((until - now) / 60000);
    return "clears in ~" + (mins >= 60 ? Math.floor(mins / 60) + "h " + (mins % 60) + "m" : mins + "m");
  }

  async function refreshRiskPanel() {
    if (!els.riskContent) return;
    try {
      const status = await getJSON("/status");
      const rows = [];
      const pr = status.portfolio_risk || {};
      if (pr.kill_switch_active) {
        rows.push('<div class="panel-row risk-alert">🛑 Kill switch ACTIVE (drawdown ' +
          escapeHTML(fmtPct(pr.current_drawdown_pct)) + ")</div>");
      } else {
        rows.push('<div class="panel-row">Kill switch: off (drawdown ' +
          escapeHTML(fmtPct(pr.current_drawdown_pct)) + ")</div>");
      }
      const now = Date.now();
      Object.keys(status.strategies || {}).sort().forEach(function (id) {
        const rs = (status.strategies[id] || {}).risk_state || {};
        if (rs.circuit_breaker) {
          rows.push('<div class="panel-row risk-alert">' + escapeHTML(id) + ": CB OPEN (" +
            escapeHTML(cbUntilLabel(rs.circuit_breaker_until, now)) + ")</div>");
        }
        const pending = rs.pending_circuit_closes ? Object.keys(rs.pending_circuit_closes).length : 0;
        if (pending > 0) {
          rows.push('<div class="panel-row risk-alert">' + escapeHTML(id) +
            ": pending circuit close (" + pending + " venue(s))</div>");
        }
      });
      if (rows.length === 1) {
        rows.push('<div class="panel-row panel-muted">No active circuit breakers</div>');
      }
      els.riskContent.innerHTML = rows.join("");
    } catch (_err) {
      panelFallback(els.riskContent, "-");
    }
  }

  async function refreshRegimeStorePanel() {
    if (!els.regimeStoreContent) return;
    try {
      const resp = await getJSON("/api/regime");
      const entries = resp.regimes || [];
      if (!entries.length) {
        panelFallback(els.regimeStoreContent, "No regime store entries yet");
        return;
      }
      els.regimeStoreContent.innerHTML = entries.map(function (entry) {
        const title = [entry.symbol, entry.timeframe].filter(Boolean).join(" ") +
          (entry.platform ? " (" + entry.platform + ")" : "");
        const windows = entry.windows || {};
        const windowRows = Object.keys(windows).sort().map(function (name) {
          const win = windows[name] || {};
          const label = win.regime || "-";
          const views = [];
          if (win.adx3 && win.adx3 !== label) views.push("adx3: " + win.adx3);
          if (win.composite7 && win.composite7 !== label) views.push("c7: " + win.composite7);
          return '<div class="panel-row panel-indent">' + escapeHTML(name) + ": " +
            '<span class="regime-badge ' + regimeBadgeClass(label) + '">' +
            escapeHTML(humanizeRegimeLabel(label)) + "</span>" +
            (views.length ? ' <span class="panel-muted">' + escapeHTML(views.join(" · ")) + "</span>" : "") +
            "</div>";
        }).join("");
        return '<div class="panel-row panel-title">' + escapeHTML(title) + "</div>" + windowRows;
      }).join("");
    } catch (_err) {
      panelFallback(els.regimeStoreContent, "-");
    }
  }

  async function refreshTransitionsPanel() {
    if (!els.transitionsContent) return;
    try {
      const resp = await getJSON("/api/regime/transitions?limit=30");
      const rows = resp.transitions || [];
      if (!rows.length) {
        panelFallback(els.transitionsContent, "No transitions recorded yet");
        return;
      }
      els.transitionsContent.innerHTML = rows.map(function (row) {
        const when = row.ts ? new Date(row.ts).toLocaleString(undefined, {
          month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
        }) : "-";
        const scope = [row.symbol, row.timeframe, row.window].filter(Boolean).join(" ");
        return '<div class="panel-row"><span class="panel-muted">' + escapeHTML(when) + "</span> " +
          escapeHTML(scope) + ": " + escapeHTML(humanizeRegimeLabel(row.old_label || "-")) +
          " → <strong>" + escapeHTML(humanizeRegimeLabel(row.new_label || "-")) + "</strong>" +
          (row.alerted_at ? " 🔔" : "") + "</div>";
      }).join("");
    } catch (_err) {
      panelFallback(els.transitionsContent, "-");
    }
  }

  // ── Ops panels (#1231): leaderboard / diagnostics / cashflow / correlation /
  // dead strategies / close evaluators. Rendered in table view; every panel is
  // best-effort — a failing endpoint renders "-" and never breaks the page.

  function opsPnlClass(v) {
    return v > 0 ? "pnl-pos" : v < 0 ? "pnl-neg" : "";
  }

  async function refreshLeaderboardPanel() {
    if (!els.leaderboardBody) return;
    try {
      const resp = await getJSON("/api/leaderboard");
      const entries = resp.entries || [];
      // Restore the friendly empty copy — a prior error cycle may have
      // mutated this node to "-", and it must reflect the latest fetch.
      els.leaderboardEmpty.textContent = "No strategies to rank";
      els.leaderboardEmpty.hidden = entries.length > 0;
      els.leaderboardBody.innerHTML = entries.map(function (e, i) {
        return "<tr>" +
          "<td>" + (i + 1) + "</td>" +
          "<td>" + escapeHTML(e.id) + "</td>" +
          '<td class="' + opsPnlClass(e.pnl_pct) + '">' + escapeHTML(fmtPct(e.pnl_pct)) + "</td>" +
          '<td class="' + opsPnlClass(e.pnl) + '">' + escapeHTML(fmtMoney(e.pnl)) + "</td>" +
          "<td>" + escapeHTML(String(e.positions_opened || 0)) + "</td>" +
          "<td>" + escapeHTML((e.wins || 0) + "/" + (e.losses || 0)) + "</td>" +
          "<td>" + escapeHTML(fmtMoney(e.value)) + "</td>" +
          "</tr>";
      }).join("");
    } catch (_err) {
      els.leaderboardEmpty.hidden = false;
      els.leaderboardEmpty.textContent = "-";
      els.leaderboardBody.innerHTML = "";
    }
  }

  function diagPct(v) {
    // NULL metrics (status != ok) render as pending per #1147 semantics.
    return v === null || v === undefined ? "…" : fmtPct(v);
  }

  async function refreshDiagnosticsPanel() {
    if (!els.diagnosticsBody) return;
    try {
      const resp = await getJSON("/api/diagnostics?limit=25");
      const rows = resp.rows || [];
      els.diagnosticsEmpty.textContent = "No diagnostics rows yet";
      els.diagnosticsEmpty.hidden = rows.length > 0;
      els.diagnosticsBody.innerHTML = rows.map(function (row) {
        const when = row.closed_at ? new Date(row.closed_at).toLocaleString(undefined, {
          month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
        }) : "-";
        const pending = row.metrics_status !== "ok";
        const capture = pending || row.capture_ratio === null || row.capture_ratio === undefined
          ? (pending ? "pending" : "-")
          : fmtNumber(row.capture_ratio);
        return "<tr>" +
          "<td>" + escapeHTML(when) + "</td>" +
          "<td>" + escapeHTML(row.strategy_id) + "</td>" +
          "<td>" + escapeHTML(row.symbol || "-") + "</td>" +
          "<td>" + escapeHTML(row.side || "-") + "</td>" +
          '<td class="' + opsPnlClass(row.net_pnl) + '">' + escapeHTML(fmtMoney(row.net_pnl)) + "</td>" +
          "<td>" + escapeHTML(pending ? "pending" : diagPct(row.favorable_pct)) + "</td>" +
          "<td>" + escapeHTML(pending ? "pending" : diagPct(row.adverse_pct)) + "</td>" +
          "<td>" + escapeHTML(capture) + "</td>" +
          "<td>" + escapeHTML(row.metrics_status || "-") + "</td>" +
          "</tr>";
      }).join("");
    } catch (_err) {
      els.diagnosticsEmpty.hidden = false;
      els.diagnosticsEmpty.textContent = "-";
      els.diagnosticsBody.innerHTML = "";
    }
  }

  async function refreshCashflowPanel() {
    if (!els.cashflowContent) return;
    try {
      const resp = await getJSON("/api/cashflow");
      const rows = [];
      if (!resp.alarm_enabled) {
        rows.push('<div class="panel-row risk-alert">Journal drift alarm operator-disabled</div>');
      }
      (resp.wallets || []).forEach(function (wallet) {
        const label = wallet.platform + "/" + wallet.account;
        // Badge keys off the RUNTIME basis (what actually drove this cycle's
        // drift alarm), not structural eligibility — eligibility alone can
        // overclaim during a transient fetch miss.
        let badge;
        if (wallet.shadow_only) {
          badge = '<span class="ops-badge ops-badge--shadow" title="Shadow-only journal — never the live drift basis (#1100)">shadow-only</span>';
        } else if (wallet.basis === "journal") {
          badge = '<span class="ops-badge ops-badge--live">LIVE basis</span>';
        } else if (wallet.basis === "pending") {
          badge = '<span class="ops-badge ops-badge--shadow" title="Transient journal fetch miss — trade ledger governs this cycle">pending (transient miss)</span>';
        } else if (wallet.basis === "disabled") {
          badge = '<span class="ops-badge ops-badge--shadow" title="GO_TRADER_CASHFLOW_JOURNAL_ALARM operator kill switch">alarm disabled</span>';
        } else if (wallet.basis === "trade_ledger") {
          badge = '<span class="ops-badge ops-badge--shadow">fallback (trade ledger)</span>';
        } else if (wallet.live_basis_eligible) {
          badge = '<span class="ops-badge ops-badge--shadow" title="Journal is eligible as the live basis; no reconcile cycle recorded since restart">eligible (awaiting cycle)</span>';
        } else {
          badge = '<span class="ops-badge ops-badge--shadow">fallback (trade ledger)</span>';
        }
        const detail = "settled " + fmtMoney(wallet.settled_sum) + " · " + wallet.entry_count + " events" +
          (wallet.incomplete ? " · INCOMPLETE" : "") +
          (wallet.baseline_set ? "" : " · no baseline");
        rows.push('<div class="panel-row panel-title">' + escapeHTML(label) + " " + badge + "</div>" +
          '<div class="panel-row panel-indent panel-muted">' + escapeHTML(detail) + "</div>");
      });
      (resp.drift || []).forEach(function (d) {
        rows.push('<div class="panel-row risk-alert">Drift: ' + escapeHTML(d.wallet) +
          " (" + d.cycles + " cycles" +
          (d.orphan_coins && d.orphan_coins.length ? ", orphans: " + escapeHTML(d.orphan_coins.join(", ")) : "") +
          (d.alerted ? ", alerted" : "") + ")</div>");
      });
      if (!rows.length) {
        rows.push('<div class="panel-row panel-muted">No journal wallets ingested yet</div>');
      } else if (!(resp.drift || []).length) {
        rows.push('<div class="panel-row panel-muted">No wallet drift — all shared wallets reconcile</div>');
      }
      els.cashflowContent.innerHTML = rows.join("");
    } catch (_err) {
      panelFallback(els.cashflowContent, "-");
    }
  }

  async function refreshCorrelationPanel() {
    if (!els.correlationContent) return;
    try {
      const resp = await getJSON("/api/correlation");
      const snap = resp.correlation;
      if (!snap) {
        panelFallback(els.correlationContent, "No correlation snapshot yet");
        return;
      }
      const rows = [];
      rows.push('<div class="panel-row">Gross exposure: ' + escapeHTML(fmtMoney(snap.portfolio_gross_usd)) + "</div>");
      (snap.warnings || []).forEach(function (warning) {
        rows.push('<div class="panel-row risk-alert">⚠️ ' + escapeHTML(warning) + "</div>");
      });
      const assets = Object.keys(snap.assets || {}).sort(function (a, b) {
        return (snap.assets[b].concentration_pct || 0) - (snap.assets[a].concentration_pct || 0);
      });
      assets.forEach(function (asset) {
        const e = snap.assets[asset] || {};
        rows.push('<div class="panel-row panel-indent">' + escapeHTML(asset) + ": net " +
          escapeHTML(fmtMoney(e.net_delta_usd)) + ' <span class="panel-muted">(' +
          escapeHTML(fmtPct(e.concentration_pct)) + " concentration)</span></div>");
      });
      els.correlationContent.innerHTML = rows.join("");
    } catch (_err) {
      panelFallback(els.correlationContent, "-");
    }
  }

  async function refreshDeadStrategiesPanel() {
    if (!els.deadStrategiesContent) return;
    try {
      const resp = await getJSON("/api/strategies/dead");
      const dead = resp.dead || [];
      if (!dead.length) {
        panelFallback(els.deadStrategiesContent, "All strategies have opened at least one position");
        return;
      }
      els.deadStrategiesContent.innerHTML =
        '<div class="panel-row panel-muted">' + dead.length + " of " + (resp.total || 0) +
        " strategies never opened a position</div>" +
        dead.map(function (id) {
          return '<div class="panel-row panel-indent">' + escapeHTML(id) + "</div>";
        }).join("");
    } catch (_err) {
      panelFallback(els.deadStrategiesContent, "-");
    }
  }

  async function refreshClosingStrategiesPanel() {
    if (!els.closingStrategiesContent) return;
    try {
      const resp = await getJSON("/api/closing-strategies");
      const evaluators = resp.evaluators || [];
      if (!evaluators.length) {
        panelFallback(els.closingStrategiesContent, "No close evaluators registered");
        return;
      }
      els.closingStrategiesContent.innerHTML = evaluators.map(function (ev) {
        const overrides = ev.user_overrides ? Object.keys(ev.user_overrides).sort() : [];
        return '<div class="panel-row panel-title">' + escapeHTML(ev.name) +
          (overrides.length
            ? ' <span class="ops-badge ops-badge--shadow" title="user_defaults.close overrides: ' +
              escapeHTML(overrides.join(", ")) + '">overridden</span>'
            : "") + "</div>" +
          '<div class="panel-row panel-indent panel-muted">' + escapeHTML(ev.description || "-") +
          " · " + escapeHTML((ev.platforms || []).join(", ") || "all platforms") + "</div>";
      }).join("");
    } catch (_err) {
      panelFallback(els.closingStrategiesContent, "-");
    }
  }

  function refreshOpsPanels() {
    return Promise.all([
      refreshLeaderboardPanel(),
      refreshDiagnosticsPanel(),
      refreshCashflowPanel(),
      refreshCorrelationPanel(),
      refreshDeadStrategiesPanel(),
      refreshClosingStrategiesPanel(),
    ]);
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
        await Promise.all([refreshOverview(), refreshOpsPanels()]);
        return;
      }
      await Promise.all([
        refreshChart(),
        refreshStatus(),
        refreshRiskPanel(),
        refreshRegimeStorePanel(),
        refreshTransitionsPanel(),
        loadTunerConfig(),
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
  if (els.tunerForm) {
    els.tunerForm.addEventListener("input", onTunerFieldChange);
    els.tunerForm.addEventListener("change", onTunerFieldChange);
  }
  if (els.tunerReset) {
    els.tunerReset.addEventListener("click", resetTunerToLive);
  }
  if (els.pauseToggle) {
    els.pauseToggle.addEventListener("click", function () {
      togglePause().catch(handleRefreshError);
    });
  }
  if (els.tradeOpenButton) {
    els.tradeOpenButton.addEventListener("click", function () {
      tradeOpen().catch(handleRefreshError);
    });
  }
  if (els.tradeAddButton) {
    els.tradeAddButton.addEventListener("click", function () {
      tradeAdd().catch(handleRefreshError);
    });
  }
  if (els.positions) {
    els.positions.addEventListener("click", function (event) {
      const button = event.target.closest("[data-trade-action]");
      if (!button) return;
      const action = button.dataset.tradeAction;
      let run;
      if (action === "close" || action === "force-close") {
        run = tradeClose(action);
      } else if (action === "update-sl") {
        run = tradeUpdateSL();
      } else if (action === "cancel-sl") {
        run = tradeCancelSL();
      }
      if (run) {
        run.catch(handleRefreshError);
      }
    });
  }
  if (els.addStratButton) {
    els.addStratButton.addEventListener("click", function () {
      structuralAddStrategy().catch(handleRefreshError);
    });
  }
  if (els.removeStrategyButton) {
    els.removeStrategyButton.addEventListener("click", function () {
      structuralPerStrategy("remove-strategy").catch(handleRefreshError);
    });
  }
  if (els.paperToLiveButton) {
    els.paperToLiveButton.addEventListener("click", function () {
      structuralPerStrategy("paper-to-live").catch(handleRefreshError);
    });
  }
  if (els.applyRegimeGateButton) {
    els.applyRegimeGateButton.addEventListener("click", function () {
      structuralPerStrategy("apply-regime-gate").catch(handleRefreshError);
    });
  }
  if (els.ratchetNotifySelect) {
    els.ratchetNotifySelect.addEventListener("change", function () {
      changeStrategyRatchetNotify().catch(handleRefreshError);
    });
  }
  if (els.globalRatchetSelect) {
    els.globalRatchetSelect.addEventListener("change", function () {
      changeGlobalRatchetNotify().catch(handleRefreshError);
    });
  }
  if (els.tunerApply) {
    els.tunerApply.addEventListener("click", function () {
      if (!state.tuner.config) return;
      const restart = tunerApplyRequiresRestartClient();
      if (els.tunerConfirmText) {
        els.tunerConfirmText.textContent = restart
          ? "This writes tuned parameters to the live config file. Restart go-trader afterward — hot reload cannot apply most tuner fields."
          : "This writes tuned parameters to the live config file. Send SIGHUP to reload when ready.";
      }
      if (els.tunerConfirmDialog && typeof els.tunerConfirmDialog.showModal === "function") {
        els.tunerConfirmDialog.showModal();
      } else if (window.confirm(els.tunerConfirmText ? els.tunerConfirmText.textContent : "Apply tuned config?")) {
        applyTunerConfig().catch(handleRefreshError);
      }
    });
  }
  if (els.tunerConfirmDialog) {
    els.tunerConfirmDialog.addEventListener("close", function () {
      if (els.tunerConfirmDialog.returnValue === "apply") {
        applyTunerConfig().catch(handleRefreshError);
      }
    });
  }
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
