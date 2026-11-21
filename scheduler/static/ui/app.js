(function () {
  function stableJSON(value) {
    if (Array.isArray(value)) return "[" + value.map(stableJSON).join(",") + "]";
    if (value && typeof value === "object") {
      return "{" + Object.keys(value).sort().map(function (key) {
        return JSON.stringify(key) + ":" + stableJSON(value[key]);
      }).join(",") + "}";
    }
    return JSON.stringify(value);
  }

  function sameValue(a, b) {
    return stableJSON(a) === stableJSON(b);
  }

  function hasOwn(object, key) {
    return !!object && Object.prototype.hasOwnProperty.call(object, key);
  }

  function tuningParamDiff(row, current) {
    const patchOpen = row && row.patch && row.patch.open_strategy;
    const replacement = !!patchOpen && hasOwn(patchOpen, "params") &&
      !!patchOpen.params && typeof patchOpen.params === "object" && !Array.isArray(patchOpen.params);
    const proposed = replacement ? patchOpen.params : ((row && row.params) || {});
    const currentParams = current || {};
    const candidateKeys = replacement
      ? Array.from(new Set(Object.keys(currentParams).concat(Object.keys(proposed))))
      : Object.keys(proposed);
    const keys = candidateKeys.sort().filter(function (key) {
      return !sameValue(currentParams[key], proposed[key]);
    });
    return { keys: keys, proposed: proposed, replacement: replacement };
  }

  function tuningBaselineState(result, currentOpen, defaultParams) {
    const hasOpenName = !!result && typeof result.open_strategy === "string" &&
      result.open_strategy.trim() !== "";
    const hasBaseline = !!result && hasOwn(result, "baseline_params") &&
      !!result.baseline_params && typeof result.baseline_params === "object" &&
      !Array.isArray(result.baseline_params);
    if (!hasOpenName || !hasBaseline) return "unknown";
    const live = currentOpen || {};
    const defaults = defaultParams || {};
    const liveEffective = Object.assign({}, defaults, live.params || {});
    const baselineEffective = Object.assign({}, defaults, result.baseline_params);
    return live.name === result.open_strategy && sameValue(liveEffective, baselineEffective)
      ? "current"
      : "drifted";
  }

  function tuningDetailLoadAction(activeRunID, detailLoading, loadingRunID) {
    if (!activeRunID) return "idle";
    if (!detailLoading) return "start";
    return loadingRunID === activeRunID ? "skip" : "queue";
  }

  function tuningApplyButtonState(eligibility, appliedAt) {
    switch (eligibility) {
      case "eligible":
        return { enabled: true, label: "Apply", reason: "" };
      case "already_applied":
        return {
          enabled: false,
          label: "Applied",
          reason: appliedAt ? ("Promoted at " + appliedAt) : "Already promoted",
        };
      case "baseline_drifted":
        return {
          enabled: false,
          label: "Apply",
          reason: "Live values changed since this run. Re-run tuning before applying.",
        };
      case "legacy_artifact":
        return {
          enabled: false,
          label: "Apply",
          reason: "This run is too old to apply safely. Re-run tuning.",
        };
      case "not_survivor":
        return {
          enabled: false,
          label: "Apply",
          reason: "Only ranked survivors can be applied.",
        };
      case "config_unavailable":
        return {
          enabled: false,
          label: "Apply",
          reason: "Live configuration is unavailable right now.",
        };
      default:
        return {
          enabled: false,
          label: "Apply",
          reason: "Apply is unavailable for this suggestion.",
        };
    }
  }

  function tuningApplyConfirmMessage(strategyID, suggestionKey, config) {
    var msg = "Apply tuning suggestion " + suggestionKey +
      " to strategy " + strategyID + "? This replaces the live open strategy parameters.";
    if (config && config.has_open_position && !config.close_strategy) {
      msg += " This strategy currently has an open trade and uses the same open logic for exits, so applying will also change how that live trade is managed.";
    }
    return msg;
  }

  function blockedStorage() {
    return {
      read: function () { return { ok: false }; },
      write: function () { return { ok: false }; },
      remove: function () { return { ok: false }; },
    };
  }

  function createTokenState(storage, key) {
    let override = null;
    let lastSubmitted = null;
    let generation = 0;
    return {
      current: function () {
        if (override !== null) return override;
        const read = storage.read(key);
        if (read.ok) return read.value === null ? "" : read.value;
        return lastSubmitted === null ? "" : lastSubmitted;
      },
      generation: function () {
        return generation;
      },
      submit: function (raw) {
        const value = String(raw === undefined || raw === null ? "" : raw).trim();
        generation += 1;
        lastSubmitted = value;
        const change = value ? storage.write(key, value) : storage.remove(key);
        const check = change.ok ? storage.read(key) : { ok: false };
        const persisted = change.ok && check.ok && (value ? check.value === value : check.value === null);
        override = persisted ? null : value;
        return { persisted: persisted, generation: generation };
      },
    };
  }

  function positiveFinite(value) {
    return typeof value === "number" && isFinite(value) && value > 0;
  }

  function recognizedPartition(partition) {
    return partition === "live" || partition === "paper" || /^paper:\S+$/.test(typeof partition === "string" ? partition : "");
  }

  function bookPositionLines(strategy, status, candles) {
    function none(reason) {
      return { lines: [], reason: reason };
    }
    if (!strategy || !status || !candles) return none("missing_input");
    if (!recognizedPartition(strategy.partition)) return none("unknown_partition");
    if (status.id !== strategy.id || candles.strategy_id !== strategy.id) return none("strategy_mismatch");
    let source = typeof candles.source === "string" ? candles.source : "";
    if (source.endsWith(":cached")) source = source.slice(0, -":cached".length);
    if (!source || source === "trades") return none("ambiguous_candles");
    const symbol = typeof status.symbol === "string" ? status.symbol : "";
    if (!symbol || symbol !== strategy.symbol) return none("symbol_mismatch");
    const positions = status.positions && typeof status.positions === "object" && !Array.isArray(status.positions)
      ? status.positions
      : {};
    const matches = Object.keys(positions).filter(function (key) {
      const pos = positions[key];
      return !!pos && typeof pos === "object" && (key === symbol || pos.symbol === symbol);
    });
    if (matches.some(function (key) { return !!positions[key].hedge_for; })) return none("hedge_row");
    if (matches.length === 0) return none("flat");
    if (matches.length !== 1) return none("ambiguous_position");
    const pos = positions[matches[0]];
    if (matches[0] !== symbol || (pos.symbol !== undefined && pos.symbol !== symbol)) return none("ambiguous_position");
    if (!positiveFinite(pos.quantity)) return none("flat");
    if (typeof pos.position_id !== "string" || pos.position_id === "") return none("no_position_id");
    const lines = [];
    if (positiveFinite(pos.avg_cost)) {
      lines.push({ price: pos.avg_cost, color: "#2563eb", title: "Book entry", style: "dashed" });
    }
    const stopOrderKnown = Number.isInteger(pos.stop_loss_oid) && pos.stop_loss_oid > 0;
    if (positiveFinite(pos.stop_loss_trigger_px) && (strategy.partition !== "live" || stopOrderKnown)) {
      lines.push({ price: pos.stop_loss_trigger_px, color: "#c23b3b", title: "Book-tracked stop", style: "dashed" });
    }
    return { lines: lines, reason: "" };
  }

  function bearerHeaders(token) {
    if (!token || !/^[\t\x20-\x7e\x80-\xff]+$/.test(token)) return {};
    return { Authorization: "Bearer " + token };
  }

  const tuningLogic = {
    baselineState: tuningBaselineState,
    paramDiff: tuningParamDiff,
    sameValue: sameValue,
    detailLoadAction: tuningDetailLoadAction,
    applyButtonState: tuningApplyButtonState,
    applyConfirmMessage: tuningApplyConfirmMessage,
    blockedStorage: blockedStorage,
    bookPositionLines: bookPositionLines,
    createTokenState: createTokenState,
    bearerHeaders: bearerHeaders,
  };
  if (typeof module !== "undefined" && module.exports) module.exports = tuningLogic;
  if (typeof document === "undefined") return;

  const SIDEBAR_STORAGE_KEY = "goTraderSidebarOpen";
  const MOBILE_SIDEBAR_MQ = "(max-width: 980px)";
  const VIEW_MODE_KEY = "goTraderViewMode";
  const STATUS_TOKEN_KEY = "goTraderStatusToken";
  const AUTH_CHECKING_TEXT = "Checking the token…";
  const AUTH_CHECK_FAILED_TEXT = "The token check did not finish. Try again.";
  const storageLib = window.GoTraderStorage && typeof window.GoTraderStorage.createGuardedStorage === "function"
    ? window.GoTraderStorage
    : null;
  const guardedStorage = storageLib
    ? storageLib.createGuardedStorage(function () { return window.localStorage; })
    : blockedStorage();
  const chartPreferences = storageLib && typeof storageLib.createPreferenceAccessor === "function"
    ? storageLib.createPreferenceAccessor(guardedStorage, storageLib.CHART_PREFERENCES_KEY)
    : null;
  const statusToken = createTokenState(guardedStorage, STATUS_TOKEN_KEY);

  if (document.body.dataset.page === "tuning") {
    initTuningPage();
    return;
  }

  const state = {
    strategies: [],
    overviewRows: [],
    activeID: "",
    viewMode: "detail",
    sortKey: "id",
    modeFilter: "all",
    partitionFilter: "all",
    partitions: [],
    sortDir: "asc",
    chart: null,
    timer: 0,
    sparklines: {},
    selectionEpoch: 0,
    lanes: {
      candles: { issued: 0, accepted: 0 },
      older: { issued: 0, accepted: 0 },
      trades: { issued: 0, accepted: 0 },
      config: { issued: 0, accepted: 0 },
      apply: { issued: 0, accepted: 0 },
      pause: { issued: 0, accepted: 0 },
      notify: { issued: 0, accepted: 0 },
      status: { issued: 0, accepted: 0 },
      feedback: { issued: 0, accepted: 0 },
    },
    chartData: null,
    statusData: null,
    chartSeries: null,
    chartEpoch: 0,
    chartTimeframe: "",
    chartOlder: { inFlight: false, timer: 0 },
    chartBackoff: { until: 0, attempt: 0 },
    tradeHistory: null,
    tuner: {
      config: null,
      overrides: {},
      liveMarkers: [],
      simulatedMarkers: [],
      liveRefusal: "",
      simulatedRefusal: "",
      previewActive: false,
      simulateTimer: 0,
      loading: false,
      previewSeq: 0,
      previewFor: "",
      pendingApply: null,
      applyTicket: null,
      notice: "",
      ratchet: { confirmed: "inherit", held: null, ticket: null },
    },
    lateResults: [],
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
    chartFailure: document.getElementById("chart-failure"),
    chartTimeframe: document.getElementById("chart-timeframe"),
    chartSource: document.getElementById("chart-source"),
    chartNote: document.getElementById("chart-note"),
    chartWrap: document.querySelector(".chart-wrap"),
    chartFrame: document.getElementById("chart-frame"),
    chartToolbar: document.getElementById("chart-toolbar"),
    tradeHistoryBody: document.getElementById("trade-history-body"),
    tradeHistoryEmpty: document.getElementById("trade-history-empty"),
    tradeHistoryTable: document.querySelector(".trade-history-table"),
    darkToggle: document.getElementById("dark-mode-toggle"),
    darkIcon: document.getElementById("dark-mode-icon"),
    refresh: document.getElementById("refresh-button"),
    viewMode: document.getElementById("view-mode-button"),
    partitionSelect: document.getElementById("partition-select"),
    interval: document.getElementById("refresh-interval"),
    statusDot: document.getElementById("status-dot"),
    statusLabel: document.getElementById("status-label"),
    authPanel: document.getElementById("auth-panel"),
    authToken: document.getElementById("auth-token"),
    authMessage: document.getElementById("auth-message"),
    dashboardMessage: document.getElementById("dashboard-message"),
    dashboardMessageList: document.getElementById("dashboard-message-list"),
    dashboardMessageDismiss: document.getElementById("dashboard-message-dismiss"),
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
    tradePaperCloseAll: document.getElementById("trade-paper-close-all"),
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
    addStratOpenParams: document.getElementById("add-strat-open-params"),
    addQuoteFile: document.getElementById("add-quote-file"),
    addQuoteRestart: document.getElementById("add-quote-restart"),
    addQuoteButton: document.getElementById("add-quote-button"),
    addQuoteMessage: document.getElementById("add-quote-message"),
    leaderboardBody: document.getElementById("leaderboard-body"),
    leaderboardEmpty: document.getElementById("leaderboard-empty"),
    diagnosticsBody: document.getElementById("diagnostics-body"),
    diagnosticsEmpty: document.getElementById("diagnostics-empty"),
    cashflowContent: document.getElementById("cashflow-content"),
    correlationContent: document.getElementById("correlation-content"),
    deadStrategiesContent: document.getElementById("dead-strategies-content"),
    closingStrategiesContent: document.getElementById("closing-strategies-content"),
    feedbackPanel: document.getElementById("feedback-analysis-panel"),
    feedbackStatus: document.getElementById("feedback-analysis-status"),
    feedbackSummary: document.getElementById("feedback-analysis-summary"),
    feedbackList: document.getElementById("feedback-analysis-list"),
    feedbackMessage: document.getElementById("feedback-analysis-message"),
    feedbackResearchDialog: document.getElementById("feedback-research-dialog"),
    feedbackResearchStrategy: document.getElementById("feedback-research-strategy"),
    feedbackResearchGrid: document.getElementById("feedback-research-grid"),
    feedbackResearchGo: document.getElementById("feedback-research-go"),
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
    return bearerHeaders(statusToken.current());
  }

  function tokenStampCurrent(err) {
    return !err || err.tokenGeneration === undefined || err.tokenGeneration === statusToken.generation();
  }

  async function postJSON(url, body) {
    const generation = statusToken.generation();
    const res = await fetch(url, {
      method: "POST",
      headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
      body: JSON.stringify(body || {}),
    });
    if (!res.ok) {
      const text = await res.text();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      err.tokenGeneration = generation;
      throw err;
    }
    return res.json();
  }

  async function getJSON(url) {
    const generation = statusToken.generation();
    const res = await fetch(url, { headers: authHeaders() });
    if (!res.ok) {
      const text = await res.text();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      err.tokenGeneration = generation;
      throw err;
    }
    return res.json();
  }

  // Every dashboard read carries the selected partition, so the process
  // answers with one partition's view instead of the front end hiding rows a
  // page or a total already counted.
  function withPartition(url) {
    if (state.partitionFilter === "all") return url;
    return url + (url.indexOf("?") === -1 ? "?" : "&") +
      "partition=" + encodeURIComponent(state.partitionFilter);
  }

  function stampErrorPartition(err, partition) {
    if (err && typeof err === "object" && err.partition === undefined) {
      err.partition = partition;
    }
    return err;
  }

  // A reload can retire a source while the selector still names it. The server
  // answers 404 for a partition it does not own, and the dashboard falls back
  // to every partition rather than showing a stale view. Every 404 of a
  // partitioned read retries bare, so sibling reads inside one refresh recover
  // in the same cycle instead of painting an error after the first one resets
  // the selection.
  async function getJSONForPartition(url) {
    const requested = state.partitionFilter;
    if (requested === "all") {
      try {
        return { data: await getJSON(url), partition: "all" };
      } catch (err) {
        throw stampErrorPartition(err, "all");
      }
    }
    try {
      return { data: await getJSON(withPartition(url)), partition: requested };
    } catch (err) {
      if (err && err.status === 404) {
        if (state.partitionFilter === requested) {
          state.partitionFilter = "all";
          renderPartitionSelect();
        }
        try {
          return { data: await getJSON(url), partition: "all" };
        } catch (fallbackErr) {
          throw stampErrorPartition(fallbackErr, "all");
        }
      }
      throw stampErrorPartition(err, requested);
    }
  }

  // A response is rendered only under the selection it was fetched for, so a
  // reply that lands after the operator switched partitions is dropped rather
  // than drawn under the new selection.
  function partitionStillSelected(partition) {
    return partition === state.partitionFilter;
  }

  function partitionSelected(partition) {
    return state.partitionFilter === "all" || state.partitionFilter === partition;
  }

  // An alert row whose owner the process cannot name is shown under every
  // selection: hiding a circuit breaker because its strategy left the roster
  // would drop a protective alert silently.
  function partitionRowVisible(partition) {
    return !partition || partitionSelected(partition);
  }

  function renderPartitionSelect() {
    const el = els.partitionSelect;
    if (!el) return;
    const list = state.partitions || [];
    // One partition is the layout that always shipped, so the control stays
    // out of the way until a source is actually folded in.
    el.hidden = list.length < 2;
    if (el.hidden) {
      // A hidden selector must not keep scoping the panels: a selection the
      // operator can no longer see or change would filter every panel against
      // a partition this process has stopped owning.
      state.partitionFilter = "all";
      el.innerHTML = "";
      return;
    }
    const known = list.some(function (p) { return p.partition === state.partitionFilter; });
    if (state.partitionFilter !== "all" && !known) {
      state.partitionFilter = "all";
    }
    el.innerHTML = '<option value="all">All partitions</option>' +
      list.map(function (p) {
        return '<option value="' + escapeHTML(p.partition) + '">' +
          escapeHTML(p.label || p.partition) + "</option>";
      }).join("");
    el.value = state.partitionFilter;
  }

  function setPartitionFilter(value) {
    if (state.partitionFilter === value) return;
    state.partitionFilter = value;
    renderPartitionSelect();
    // A strategy outside the new partition cannot stay selected: its detail
    // panels would read a partition the operator is no longer looking at.
    const active = state.strategies.filter(function (item) {
      return item.id === state.activeID;
    })[0];
    if (active && !partitionSelected(active.partition)) {
      setActiveStrategy("");
    }
    reloadForPartition().catch(handleRefreshError);
  }

  async function reloadForPartition() {
    const read = await getJSONForPartition("/api/strategies");
    if (!partitionStillSelected(read.partition)) return;
    state.strategies = read.data.strategies || [];
    renderStrategies();
    if (!state.activeID && state.strategies.length) {
      await selectStrategy(state.strategies[0].id);
      return;
    }
    await refreshAll();
  }

  function initTuningPage() {
    const pageState = {
      strategies: [],
      configs: {},
      configErrors: {},
      drafts: {},
      runs: [],
      activeRunID: "",
      detailLoading: false,
      detailLoadingRunID: "",
      detailReloadPending: false,
      pollTimer: 0,
      authFailures: 0,
    };
    const pageEls = {
      auth: document.getElementById("tuning-auth"),
      authForm: document.getElementById("tuning-auth-form"),
      authToken: document.getElementById("tuning-auth-token"),
      authMessage: document.getElementById("tuning-auth-message"),
      refresh: document.getElementById("tuning-refresh"),
      form: document.getElementById("tuning-launch-form"),
      strategies: document.getElementById("tuning-strategies"),
      overrides: document.getElementById("tuning-overrides"),
      launch: document.getElementById("tuning-launch"),
      launchMessage: document.getElementById("tuning-launch-message"),
      runs: document.getElementById("tuning-runs"),
      detail: document.getElementById("tuning-run-detail"),
      runTitle: document.getElementById("tuning-run-title"),
      runMeta: document.getElementById("tuning-run-meta"),
      runStatus: document.getElementById("tuning-run-status"),
      progress: document.getElementById("tuning-progress"),
      runError: document.getElementById("tuning-run-error"),
      results: document.getElementById("tuning-results"),
    };

    function node(tag, className, textValue) {
      const out = document.createElement(tag);
      if (className) out.className = className;
      if (textValue !== undefined) out.textContent = textValue;
      return out;
    }

    function clear(element) {
      while (element && element.firstChild) element.removeChild(element.firstChild);
    }

    function apiErrorMessage(err) {
      const raw = String((err && err.message) || "Request failed").trim();
      try {
        const parsed = JSON.parse(raw);
        if (parsed.reason && parsed.error) return parsed.reason + ": " + parsed.error;
        return parsed.error || parsed.reason || parsed.message || raw;
      } catch (_err) {
        return raw;
      }
    }

    function setLaunchMessage(message, kind) {
      pageEls.launchMessage.hidden = !message;
      pageEls.launchMessage.textContent = message || "";
      pageEls.launchMessage.className = "controls-message" + (kind ? " " + kind : "");
    }

    function isAuthError(err) {
      return !!err && (err.status === 401 || err.status === 403);
    }

    function showAuth(err, prefix) {
      if (!tokenStampCurrent(err)) return;
      pageState.authFailures += 1;
      const detail = err ? apiErrorMessage(err) : "Authorization required";
      pageEls.authMessage.textContent = (prefix ? prefix + ": " : "") + detail;
      if (pageEls.auth.hidden) {
        pageEls.authToken.value = statusToken.current();
        pageEls.auth.hidden = false;
        pageEls.authToken.focus();
      }
    }

    function handlePageError(err, prefix) {
      const current = tokenStampCurrent(err);
      if (isAuthError(err)) {
        if (!current) return;
        showAuth(err, prefix || "Authorization failed");
      } else if (current && !pageEls.auth.hidden && pageEls.authMessage.textContent === AUTH_CHECKING_TEXT) {
        pageEls.authMessage.textContent = AUTH_CHECK_FAILED_TEXT;
      }
      setLaunchMessage((prefix ? prefix + ": " : "") + apiErrorMessage(err), "error");
    }

    function selectedStrategyIDs() {
      return Array.from(pageEls.strategies.querySelectorAll("input[type=checkbox]:checked"))
        .map(function (input) { return input.value; });
    }

    function ensureDraft(id) {
      if (!pageState.drafts[id]) pageState.drafts[id] = { params: {}, freeze: {} };
      return pageState.drafts[id];
    }

    async function loadStrategyConfig(id) {
      if (pageState.configs[id] || pageState.configErrors[id] === "loading") return;
      pageState.configErrors[id] = "loading";
      renderOverrides();
      try {
        pageState.configs[id] = await getJSON("/api/strategies/" + encodeURIComponent(id) + "/config");
        delete pageState.configErrors[id];
      } catch (err) {
        pageState.configErrors[id] = apiErrorMessage(err);
        if (err.status === 401 || err.status === 403) showAuth(err, "Live config access failed");
      }
      renderOverrides();
    }

    function renderStrategyPicker() {
      clear(pageEls.strategies);
      if (!pageState.strategies.length) {
        pageEls.strategies.appendChild(node("p", "panel-muted", "No configured strategies"));
        return;
      }
      pageState.strategies.forEach(function (strategy) {
        const label = node("label", "tuning-strategy-option");
        const checkbox = node("input");
        checkbox.type = "checkbox";
        checkbox.value = strategy.id;
        checkbox.addEventListener("change", function () {
          ensureDraft(strategy.id);
          if (checkbox.checked) loadStrategyConfig(strategy.id);
          renderOverrides();
        });
        const copy = node("span", "tuning-strategy-copy");
        copy.appendChild(node("strong", "", strategy.id));
        copy.appendChild(node("small", "", [strategy.platform, strategy.symbol, strategy.timeframe].filter(Boolean).join(" · ")));
        label.appendChild(checkbox);
        label.appendChild(copy);
        pageEls.strategies.appendChild(label);
      });
    }

    function paramKeys(config) {
      const keys = new Set();
      Object.keys(config.default_params || {}).forEach(function (key) { keys.add(key); });
      Object.keys((config.open_strategy && config.open_strategy.params) || {}).forEach(function (key) { keys.add(key); });
      return Array.from(keys).sort();
    }

    function formatValue(value) {
      if (value === undefined) return "—";
      if (typeof value === "string") return value;
      return JSON.stringify(value);
    }

    function renderOverrides() {
      clear(pageEls.overrides);
      const selected = selectedStrategyIDs();
      selected.forEach(function (id) {
        const card = node("section", "tuning-override-card");
        const config = pageState.configs[id];
        const error = pageState.configErrors[id];
        card.appendChild(node("h3", "", id));
        if (!config) {
          card.appendChild(node("p", error && error !== "loading" ? "tuning-error" : "panel-muted",
            error && error !== "loading" ? error : "Loading live parameters…"));
          pageEls.overrides.appendChild(card);
          return;
        }
        card.appendChild(node("p", "panel-muted", "Open strategy: " + ((config.open_strategy || {}).name || "—")));
        const keys = paramKeys(config);
        if (!keys.length) {
          card.appendChild(node("p", "panel-muted", "This strategy exposes no tunable parameters."));
          pageEls.overrides.appendChild(card);
          return;
        }
        const draft = ensureDraft(id);
        const tableWrap = node("div", "tuning-param-scroll");
        const table = node("table", "tuning-param-table");
        const thead = node("thead");
        const headRow = node("tr");
        ["Parameter", "Live", "Freeze", "Override grid (JSON array)"].forEach(function (heading) {
          headRow.appendChild(node("th", "", heading));
        });
        thead.appendChild(headRow);
        table.appendChild(thead);
        const tbody = node("tbody");
        keys.forEach(function (key) {
          const row = node("tr");
          const params = (config.open_strategy && config.open_strategy.params) || {};
          const live = Object.prototype.hasOwnProperty.call(params, key) ? params[key] : (config.default_params || {})[key];
          row.appendChild(node("th", "", key));
          row.appendChild(node("td", "tuning-live-value", formatValue(live)));

          const freezeCell = node("td");
          const freeze = node("input");
          freeze.type = "checkbox";
          freeze.checked = !!draft.freeze[key];
          freeze.setAttribute("aria-label", "Freeze " + key);
          freezeCell.appendChild(freeze);
          row.appendChild(freezeCell);

          const gridCell = node("td");
          const grid = node("input", "tuning-grid-input");
          grid.type = "text";
          grid.spellcheck = false;
          grid.placeholder = "e.g. [10, 14, 20]";
          grid.value = draft.params[key] || "";
          grid.disabled = freeze.checked;
          grid.setAttribute("aria-label", "Override grid for " + key);
          freeze.addEventListener("change", function () {
            draft.freeze[key] = freeze.checked;
            if (freeze.checked) {
              draft.params[key] = "";
              grid.value = "";
            }
            grid.disabled = freeze.checked;
          });
          grid.addEventListener("input", function () {
            draft.params[key] = grid.value;
            if (grid.value.trim()) {
              draft.freeze[key] = false;
              freeze.checked = false;
            }
          });
          gridCell.appendChild(grid);
          row.appendChild(gridCell);
          tbody.appendChild(row);
        });
        table.appendChild(tbody);
        tableWrap.appendChild(table);
        card.appendChild(tableWrap);
        pageEls.overrides.appendChild(card);
      });
    }

    function launchPayload() {
      const ids = selectedStrategyIDs();
      if (!ids.length) throw new Error("Select at least one strategy");
      const overrides = {};
      ids.forEach(function (id) {
        if (!pageState.configs[id]) throw new Error(id + ": live parameter metadata is unavailable");
        const draft = ensureDraft(id);
        const entry = { params: {}, freeze: [] };
        Object.keys(draft.params).sort().forEach(function (key) {
          const raw = String(draft.params[key] || "").trim();
          if (!raw) return;
          let values;
          try {
            values = JSON.parse(raw);
          } catch (_err) {
            throw new Error(id + "/" + key + ": override must be a JSON array");
          }
          if (!Array.isArray(values) || !values.length) {
            throw new Error(id + "/" + key + ": override must be a non-empty JSON array");
          }
          entry.params[key] = values;
        });
        Object.keys(draft.freeze).sort().forEach(function (key) {
          if (draft.freeze[key]) entry.freeze.push(key);
        });
        if (Object.keys(entry.params).length || entry.freeze.length) overrides[id] = entry;
      });
      const payload = { strategy_ids: ids };
      if (Object.keys(overrides).length) payload.overrides = overrides;
      return payload;
    }

    function runTime(run) {
      const value = run.completed_at || run.started_at || run.created_at;
      if (!value) return "—";
      const date = new Date(value);
      return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
    }

    function renderRuns() {
      clear(pageEls.runs);
      if (!pageState.runs.length) {
        pageEls.runs.appendChild(node("p", "panel-muted", "No tuning runs yet"));
        return;
      }
      pageState.runs.forEach(function (run) {
        const button = node("button", "tuning-run-button" + (run.id === pageState.activeRunID ? " active" : ""));
        button.type = "button";
        const top = node("span", "tuning-run-button-top");
        top.appendChild(node("strong", "", (run.strategy_ids || []).join(", ") || run.id));
        top.appendChild(node("span", "tuning-status-pill status-" + run.status, run.status));
        button.appendChild(top);
        button.appendChild(node("small", "", runTime(run) + " · " + run.id));
        button.addEventListener("click", function () {
          pageState.activeRunID = run.id;
          renderRuns();
          loadRunDetail();
        });
        pageEls.runs.appendChild(button);
      });
    }

    async function loadRuns(selectNewest) {
      const data = await getJSON("/api/tuning/runs");
      pageState.runs = data.runs || [];
      if (selectNewest && !pageState.activeRunID && pageState.runs.length) {
        pageState.activeRunID = pageState.runs[0].id;
      }
      renderRuns();
    }

    function appendProgress(label, value) {
      const item = node("div", "tuning-progress-item");
      item.appendChild(node("span", "", label));
      item.appendChild(node("strong", "", value === undefined || value === null ? "—" : String(value)));
      pageEls.progress.appendChild(item);
    }

    function liveEffectiveParams(config) {
      return Object.assign({}, config.default_params || {}, (config.open_strategy && config.open_strategy.params) || {});
    }

    function bhAdjustedLabel(verdict) {
      switch (verdict) {
      case "survivor": return "BH-adjusted: passed";
      case "positive_uncorrected_only": return "BH-adjusted: raw signal did not survive correction";
      case "positive_but_not_significant": return "BH-adjusted: not significant";
      case "baseline": return "BH-adjusted: live baseline";
      default: return "BH-adjusted verdict: " + (verdict || "inconclusive");
      }
    }

    function collectEvidenceMetrics(value, path, out) {
      if (out.length >= 10 || value === null || value === undefined) return;
      if (typeof value === "number") {
        const leaf = path.split(".").pop() || "";
        if (/(mean|effect|delta|sharpe|return|permutation_p|p_value|^p$)/i.test(leaf)) {
          out.push({ label: path, value: value });
        }
        return;
      }
      if (Array.isArray(value)) {
        value.slice(0, 4).forEach(function (item, index) {
          collectEvidenceMetrics(item, path + "[" + index + "]", out);
        });
        return;
      }
      if (typeof value === "object") {
        Object.keys(value).sort().forEach(function (key) {
          collectEvidenceMetrics(value[key], path ? path + "." + key : key, out);
        });
      }
    }

    function appendDiff(tableBody, key, current, proposed) {
      const row = node("tr");
      row.appendChild(node("th", "", key));
      row.appendChild(node("td", "", formatValue(current)));
      row.appendChild(node("td", "", formatValue(proposed)));
      tableBody.appendChild(row);
    }

    function renderRankedRow(row, config, strategyID, runID) {
      const card = node("article", "tuning-suggestion");
      const heading = node("div", "tuning-suggestion-heading");
      heading.appendChild(node("strong", "", row.key || "candidate"));
      heading.appendChild(node("span", "tuning-verdict verdict-" + (row.verdict || "unknown"), row.verdict || "unknown"));
      card.appendChild(heading);
      card.appendChild(node("p", "tuning-bh-label", bhAdjustedLabel(row.verdict)));

      const current = liveEffectiveParams(config);
      const diff = tuningLogic.paramDiff(row, current);
      const proposed = diff.proposed;
      const keys = diff.keys;
      const diffTitle = node("h4", "", diff.replacement
        ? "Proposed replacement patch vs current live values"
        : "Candidate parameters vs current live values");
      card.appendChild(diffTitle);
      if (!keys.length) {
        card.appendChild(node("p", "panel-muted", "No parameter differences from the current live configuration."));
      } else {
        const wrap = node("div", "tuning-param-scroll");
        const table = node("table", "tuning-diff-table");
        const head = node("thead");
        const headRow = node("tr");
        ["Parameter", "Current live", "Proposed"].forEach(function (label) { headRow.appendChild(node("th", "", label)); });
        head.appendChild(headRow);
        table.appendChild(head);
        const body = node("tbody");
        keys.forEach(function (key) { appendDiff(body, key, current[key], proposed[key]); });
        table.appendChild(body);
        wrap.appendChild(table);
        card.appendChild(wrap);
      }

      const metrics = [];
      collectEvidenceMetrics(row.evidence || {}, "", metrics);
      if (metrics.length) {
        const metricList = node("dl", "tuning-evidence-metrics");
        metrics.forEach(function (metric) {
          metricList.appendChild(node("dt", "", metric.label));
          metricList.appendChild(node("dd", "", Number(metric.value).toPrecision(5)));
        });
        card.appendChild(metricList);
      }
      const evidence = node("details", "tuning-evidence");
      evidence.appendChild(node("summary", "", "Evidence and limitations"));
      evidence.appendChild(node("pre", "", JSON.stringify({ evidence: row.evidence || {}, limitations: row.limitations || [] }, null, 2)));
      card.appendChild(evidence);

      if (row.verdict === "survivor") {
        const applyState = tuningLogic.applyButtonState(row.apply_eligibility, row.applied_at);
        const actions = node("div", "tuning-apply-actions");
        const button = node("button", "tuner-button primary tuning-apply-button", applyState.label);
        button.type = "button";
        button.disabled = !applyState.enabled;
        if (applyState.enabled) {
          button.addEventListener("click", function () {
            applyTuningSuggestion(runID, strategyID, row.key || "", config);
          });
        }
        actions.appendChild(button);
        if (applyState.reason) {
          actions.appendChild(node("p", "tuning-apply-reason panel-muted", applyState.reason));
        }
        if (applyState.enabled && config && config.has_open_position && !config.close_strategy) {
          actions.appendChild(node("p", "tuning-apply-exit-warning panel-muted",
            "Open trade uses open-as-close exits — applying also changes how this live trade is managed."));
        }
        const status = node("p", "tuning-apply-status panel-muted");
        status.hidden = true;
        actions.appendChild(status);
        card.appendChild(actions);
      }
      return card;
    }

    async function applyTuningSuggestion(runID, strategyID, suggestionKey, config) {
      const confirmMsg = tuningLogic.applyConfirmMessage(strategyID, suggestionKey, config);
      if (!window.confirm(confirmMsg)) return;
      try {
        const resp = await postJSON("/api/tuning/apply", {
          run_id: runID,
          strategy_id: strategyID,
          suggestion_key: suggestionKey,
        });
        const reason = (resp && resp.reason) || "applied";
        const reload = (resp && resp.message) ? (" — " + resp.message) : "";
        setLaunchMessage("Apply " + reason + " for " + strategyID + " / " + suggestionKey + reload, "success");
      } catch (err) {
        const detail = apiErrorMessage(err);
        setLaunchMessage("Apply failed for " + strategyID + " / " + suggestionKey + ": " + detail, "error");
        if (err.status === 401 || err.status === 403) showAuth(err, "Apply authorization failed");
      }
      await loadRunDetail();
    }

    function renderStrategyResults(result, config, configError, runID) {
      const section = node("section", "tuning-strategy-result");
      const heading = node("div", "tuning-result-heading");
      heading.appendChild(node("h3", "", result.strategy_id || "Unknown strategy"));
      heading.appendChild(node("span", "tuning-status-pill", result.status || "unknown"));
      section.appendChild(heading);
      if (result.error || result.reason) section.appendChild(node("p", "tuning-error", result.error || result.reason));
      if (result.correction) {
        const correction = result.correction;
        section.appendChild(node("p", "tuning-correction",
          "Benjamini–Hochberg correction: m=" + formatValue(correction.m) +
          ", tests=" + formatValue(correction.tests_run) +
          ", threshold=" + formatValue(correction.effective_threshold) +
          ", survivors=" + formatValue(correction.n_survivors)));
      }
      if (configError || !config) {
        section.appendChild(node("p", "tuning-error", "Live-value diff unavailable: " + (configError || "configuration missing")));
      } else {
        const baselineState = tuningLogic.baselineState(
          result,
          config.open_strategy || {},
          config.default_params || {}
        );
        const baselineClass = baselineState === "drifted"
          ? "baseline-drifted"
          : (baselineState === "current" ? "baseline-current" : "baseline-unknown");
        const baselineMessage = baselineState === "drifted"
          ? "Baseline drifted: the run started from different live parameters. Suggestions are diffed against the values active now."
          : (baselineState === "current"
            ? "Baseline current: the run baseline still matches the live configuration."
            : "Baseline unknown: this run artifact does not include enough baseline metadata to determine drift.");
        const drift = node("p", "tuning-drift " + baselineClass, baselineMessage);
        section.appendChild(drift);
        const ranked = result.ranked || [];
        if (!ranked.length) {
          section.appendChild(node("p", "panel-muted", "No ranked suggestions for this strategy."));
        } else {
          ranked.forEach(function (row) {
            section.appendChild(renderRankedRow(row, config, result.strategy_id || "", runID || ""));
          });
        }
      }
      return section;
    }

    async function renderResults(results, expectedRunID) {
      const strategies = (results && results.strategies) || [];
      if (!strategies.length) {
        clear(pageEls.results);
        if (results && Object.keys(results).length) {
          pageEls.results.appendChild(node("pre", "tuning-raw-results", JSON.stringify(results, null, 2)));
        } else {
          pageEls.results.appendChild(node("p", "panel-muted", "Results will appear when the run produces an artifact."));
        }
        return;
      }
      const liveConfigs = {};
      const liveErrors = {};
      await Promise.all(strategies.map(async function (result) {
        try {
          liveConfigs[result.strategy_id] = await getJSON("/api/strategies/" + encodeURIComponent(result.strategy_id) + "/config");
        } catch (err) {
          liveErrors[result.strategy_id] = apiErrorMessage(err);
          if (err.status === 401 || err.status === 403) showAuth(err, "Live-value diff authorization failed");
        }
      }));
      if (pageState.activeRunID !== expectedRunID) return;
      clear(pageEls.results);
      strategies.forEach(function (result) {
        pageEls.results.appendChild(renderStrategyResults(
          result,
          liveConfigs[result.strategy_id],
          liveErrors[result.strategy_id],
          expectedRunID
        ));
      });
    }

    async function renderRunDetail(detail, expectedRunID) {
      const run = detail.run || {};
      pageEls.detail.hidden = false;
      pageEls.runTitle.textContent = (run.strategy_ids || []).join(", ") || "Tuning run";
      pageEls.runMeta.textContent = run.id + " · created " + runTime({ created_at: run.created_at });
      pageEls.runStatus.textContent = run.status || "unknown";
      pageEls.runStatus.className = "tuning-status-pill status-" + (run.status || "unknown");
      clear(pageEls.progress);
      const progress = detail.progress || {};
      appendProgress("Phase", progress.phase || run.status);
      appendProgress("Strategy", progress.strategy || "—");
      appendProgress("Progress", progress.strategy_index !== undefined && progress.n_strategies !== undefined
        ? progress.strategy_index + " / " + progress.n_strategies : "—");
      appendProgress("Candidates", progress.candidates);
      appendProgress("Survivors", progress.survivors);
      pageEls.runError.hidden = !run.error;
      pageEls.runError.textContent = run.error || "";
      await renderResults(detail.results || {}, expectedRunID);
    }

    async function loadRunDetail() {
      const action = tuningLogic.detailLoadAction(
        pageState.activeRunID,
        pageState.detailLoading,
        pageState.detailLoadingRunID
      );
      if (action === "idle" || action === "skip") {
        if (action === "skip") pageState.detailReloadPending = false;
        return;
      }
      if (action === "queue") {
        pageState.detailReloadPending = true;
        return;
      }
      const expected = pageState.activeRunID;
      pageState.detailLoading = true;
      pageState.detailLoadingRunID = expected;
      pageState.detailReloadPending = false;
      try {
        const detail = await getJSON("/api/tuning/runs/" + encodeURIComponent(expected));
        if (pageState.activeRunID === expected) await renderRunDetail(detail, expected);
      } catch (err) {
        handlePageError(err, "Run detail failed");
      } finally {
        pageState.detailLoading = false;
        pageState.detailLoadingRunID = "";
        if (pageState.detailReloadPending && pageState.activeRunID) {
          pageState.detailReloadPending = false;
          await loadRunDetail();
        }
      }
    }

    async function refreshTuning(selectNewest) {
      const generation = statusToken.generation();
      const authFailures = pageState.authFailures;
      try {
        const strategies = await getJSON("/api/strategies");
        pageState.strategies = strategies.strategies || [];
        renderStrategyPicker();
        renderOverrides();
        await loadRuns(selectNewest);
        if (pageState.activeRunID) await loadRunDetail();
        if (generation === statusToken.generation() && authFailures === pageState.authFailures) {
          pageEls.auth.hidden = true;
        }
      } catch (err) {
        handlePageError(err, "Refresh failed");
      }
    }

    pageEls.form.addEventListener("submit", async function (event) {
      event.preventDefault();
      let payload;
      try {
        payload = launchPayload();
      } catch (err) {
        setLaunchMessage(apiErrorMessage(err), "error");
        return;
      }
      pageEls.launch.disabled = true;
      setLaunchMessage("Submitting tuning run…", "");
      try {
        const run = await postJSON("/api/tuning/runs", payload);
        pageState.activeRunID = run.id;
        setLaunchMessage("Run queued: " + run.id, "success");
        await loadRuns(false);
        await loadRunDetail();
      } catch (err) {
        if (isAuthError(err) && !tokenStampCurrent(err)) {
          setLaunchMessage("Launch failed: the server refused the earlier token. Start the run again to use the new token.", "error");
        } else {
          handlePageError(err, "Launch failed");
        }
      } finally {
        pageEls.launch.disabled = false;
      }
    });

    pageEls.authForm.addEventListener("submit", function (event) {
      event.preventDefault();
      statusToken.submit(pageEls.authToken.value);
      pageEls.authMessage.textContent = AUTH_CHECKING_TEXT;
      refreshTuning(true);
    });
    pageEls.refresh.addEventListener("click", function () { refreshTuning(false); });

    refreshTuning(true);
    pageState.pollTimer = window.setInterval(async function () {
      try {
        await loadRuns(false);
        await loadRunDetail();
      } catch (err) {
        if (isAuthError(err)) showAuth(err, "Polling authorization failed");
      }
    }, 3000);
    window.addEventListener("pagehide", function () { window.clearInterval(pageState.pollTimer); });
  }

  function isDarkMode() {
    return document.documentElement.classList.contains("dark");
  }

  function applyChartTheme() {
    if (!state.chart) return;
    state.chart.applyTheme();
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
    const saved = guardedStorage.read(VIEW_MODE_KEY);
    return saved.ok && saved.value === "table" ? "table" : "detail";
  }

  function saveViewMode(mode) {
    guardedStorage.write(VIEW_MODE_KEY, mode);
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
    applyViewMode();
    saveViewMode(state.viewMode);
    refreshAll().catch(handleRefreshError);
  }

  function showChartFailure(message) {
    els.chartFailure.textContent = "Chart unavailable: " + message;
    els.chartFailure.hidden = false;
    els.empty.style.display = "none";
  }

  function hideChartFailure() {
    els.chartFailure.hidden = true;
    els.chartFailure.textContent = "";
  }

  function ensureChart() {
    if (state.chart) return state.chart;
    if (!window.GoTraderChart || typeof window.GoTraderChart.create !== "function") {
      showChartFailure("The chart script did not load. Reload the page to try again.");
      return null;
    }
    const controller = window.GoTraderChart.create({
      container: els.chart,
      toolbar: els.chartToolbar,
      fullscreenTarget: els.chartFrame,
      preferences: chartPreferences,
      exportName: function () { return state.activeID; },
      minWidth: 320,
      minHeight: 320,
      isDark: isDarkMode,
      onFailure: function (failure) {
        showChartFailure(failure.message);
      },
    });
    if (!controller.available()) {
      controller.destroy();
      return null;
    }
    controller.onNeedOlder(onChartNeedOlder);
    state.chart = controller;
    return controller;
  }

  function clearTradeHistoryTable() {
    els.tradeHistoryBody.innerHTML = "";
    delete els.tradeHistoryBody.dataset.strategyId;
    els.tradeHistoryEmpty.hidden = true;
    els.tradeHistoryTable.hidden = true;
  }

  function setActiveStrategy(id) {
    state.activeID = id;
    state.selectionEpoch += 1;
    state.chartData = null;
    state.statusData = null;
    resetChartSeries("");
    state.tradeHistory = null;
    if (state.chart) state.chart.clear();
    hideChartFailure();
    els.empty.style.display = "none";
    clearTradeHistoryTable();
    resetStrategyDetail();
    resetTunerState();
  }

  function resetStrategyDetail() {
    const strategy = activeStrategy();
    if (strategy) {
      els.title.textContent = strategy.id;
      els.subtitle.textContent = [strategy.platform, strategy.symbol, strategy.timeframe].filter(Boolean).join(" / ");
    } else if (state.activeID) {
      els.title.textContent = state.activeID;
      els.subtitle.textContent = "";
    } else {
      els.title.textContent = "Dashboard";
      els.subtitle.textContent = "Select a strategy";
    }
    updateRegimeBadge("");
    updateDivergenceBadge(null);
    updatePausedBadge(false);
    els.statusGrid.innerHTML = "";
    els.positions.innerHTML = "";
    if (els.tradePanel) els.tradePanel.hidden = true;
    setTradeMessage("");
    if (els.structuralPanel) els.structuralPanel.hidden = true;
    setStructuralMessage("");
    resetFeedbackPanel();
  }

  function takeTicket(lane) {
    const entry = state.lanes[lane];
    entry.issued += 1;
    return { id: state.activeID, epoch: state.selectionEpoch, seq: entry.issued };
  }

  function ticketSelected(ticket) {
    return ticket.id === state.activeID && ticket.epoch === state.selectionEpoch;
  }

  function acceptReply(lane, ticket) {
    const entry = state.lanes[lane];
    if (!ticketSelected(ticket) || ticket.seq <= entry.accepted) {
      return false;
    }
    entry.accepted = ticket.seq;
    return true;
  }

  function retireLane(lane) {
    const entry = state.lanes[lane];
    entry.accepted = entry.issued;
  }

  function previewTicketCurrent(ticket) {
    return ticketSelected(ticket) && ticket.seq === state.tuner.previewSeq;
  }

  function invalidatePreview() {
    state.tuner.previewSeq += 1;
    state.tuner.previewFor = "";
    state.tuner.loading = false;
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
        button.querySelector(".strategy-id").textContent = (strategy.engine_hold ? "[held] " : "") + (strategy.paused ? "⏸ " : "") + strategy.id;
        if (strategy.paused) {
          button.title = "Paused — position-increasing signals held";
        }
        if (strategy.engine_hold) {
          button.title = "Held (" + strategy.engine_hold.reason + "): " + strategy.engine_hold.text + " — " + strategy.engine_hold.tracking_url;
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
    setActiveStrategy(id);
    renderStrategies();
    if (opts.switchToDetail) {
      state.viewMode = "detail";
      applyViewMode();
      saveViewMode(state.viewMode);
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
    state.tuner.liveRefusal = "";
    state.tuner.simulatedRefusal = "";
    state.tuner.previewActive = false;
    invalidatePreview();
    if (state.tuner.simulateTimer) {
      clearTimeout(state.tuner.simulateTimer);
      state.tuner.simulateTimer = 0;
    }
    state.tuner.pendingApply = null;
    state.tuner.applyTicket = null;
    state.tuner.notice = "";
    state.tuner.ratchet = { confirmed: "inherit", held: null, ticket: null };
    if (els.tunerApply) {
      els.tunerApply.disabled = false;
    }
    if (els.tunerConfirmDialog && els.tunerConfirmDialog.open) {
      els.tunerConfirmDialog.close("cancel");
    }
    updateTunerStatus();
    if (els.tunerPanel) {
      els.tunerPanel.hidden = true;
    }
    if (els.tunerForm) {
      els.tunerForm.innerHTML = "";
    }
    updateControlsPanel(null);
    setControlsMessage("");
  }

  function currentTunerConfig() {
    const owned = state.tuner.config;
    if (!owned || owned.strategyID !== state.activeID || owned.epoch !== state.selectionEpoch) {
      return null;
    }
    return owned.resp;
  }

  function tunerHasOverrides() {
    return Object.keys(state.tuner.overrides).length > 0;
  }

  function updateTunerStatus() {
    if (!els.tunerStatus) return;
    if (state.tuner.notice) {
      els.tunerStatus.textContent = state.tuner.notice;
      els.tunerStatus.className = "tuner-status preview";
      return;
    }
    if (state.tuner.loading) {
      els.tunerStatus.textContent = "Simulating…";
      els.tunerStatus.className = "tuner-status preview";
      return;
    }
    if (tunerHasOverrides()) {
      const notes = [];
      if (state.tuner.liveRefusal) {
        notes.push("live markers unavailable: " + state.tuner.liveRefusal);
      }
      if (state.tuner.simulatedRefusal) {
        notes.push("simulated markers unavailable: " + state.tuner.simulatedRefusal);
      }
      els.tunerStatus.textContent = notes.length
        ? "Preview active; " + notes.join("; ")
        : "Preview active";
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

  const TUNER_GROUP_LABELS = {
    runtime: "Runtime",
    risk: "Risk",
    open_params: "Indicator params",
  };

  function tunerFieldShape(field) {
    return stableJSON({
      type: String(field.type || ""),
      options: field.type === "select" ? (field.options || []).map(String) : null,
    });
  }

  function findTunerField(config, key) {
    const fields = config && config.editable_fields ? config.editable_fields : [];
    return fields.find(function (field) { return field.key === key; }) || null;
  }

  function pruneTunerDraft(previous, next) {
    const dropped = [];
    Object.keys(state.tuner.overrides).forEach(function (key) {
      const before = findTunerField(previous, key);
      const after = findTunerField(next, key);
      if (!before || !after || tunerFieldShape(before) !== tunerFieldShape(after)) {
        delete state.tuner.overrides[key];
        dropped.push(key);
      }
    });
    return dropped.sort();
  }

  function tunerFieldValue(field) {
    return hasOwn(state.tuner.overrides, field.key) ? state.tuner.overrides[field.key] : field.value;
  }

  function createTunerFieldElement(field) {
    const label = document.createElement("label");
    label.className = "tuner-field";
    label.dataset.key = field.key;
    label.dataset.shape = tunerFieldShape(field);
    label.appendChild(document.createElement("span"));
    let control;
    if (field.type === "boolean") {
      control = document.createElement("input");
      control.type = "checkbox";
    } else if (field.type === "select") {
      control = document.createElement("select");
      (field.options || []).forEach(function (opt) {
        const option = document.createElement("option");
        option.value = String(opt);
        option.textContent = String(opt);
        control.appendChild(option);
      });
    } else {
      control = document.createElement("input");
      control.type = "number";
      control.step = "any";
    }
    control.dataset.key = field.key;
    label.appendChild(control);
    return label;
  }

  function writeTunerFieldElement(label, field, keepInput) {
    label.firstChild.textContent = field.label || field.key;
    if (keepInput) return;
    const control = label.lastChild;
    const value = tunerFieldValue(field);
    if (field.type === "boolean") {
      control.checked = !!value;
    } else if (field.type === "select") {
      const match = Array.from(control.options).findIndex(function (option) {
        return option.value === String(value);
      });
      control.selectedIndex = match >= 0 ? match : 0;
    } else {
      control.value = value === null || value === undefined ? "" : String(value);
    }
  }

  function renderTunerForm(config, options) {
    if (!els.tunerForm || !els.tunerPanel) return;
    const force = !!(options && options.force);
    const fields = config && config.editable_fields ? config.editable_fields : [];
    if (!fields.length) {
      els.tunerPanel.hidden = true;
      els.tunerForm.replaceChildren();
      return;
    }
    els.tunerPanel.hidden = false;
    const active = document.activeElement;
    const focused = active && active !== els.tunerForm && els.tunerForm.contains(active)
      ? active.closest("label.tuner-field")
      : null;
    const existing = {};
    Array.from(els.tunerForm.querySelectorAll("label.tuner-field")).forEach(function (label) {
      existing[label.dataset.key] = label;
    });
    const used = new Set();
    const desired = [];
    const groups = groupEditableFields(fields);
    ["runtime", "risk", "open_params"].forEach(function (groupKey) {
      const list = groups[groupKey] || [];
      if (!list.length) return;
      const heading = document.createElement("div");
      heading.className = "tuner-group-label";
      heading.textContent = TUNER_GROUP_LABELS[groupKey] || groupKey;
      desired.push(heading);
      list.forEach(function (field) {
        let label = hasOwn(existing, field.key) ? existing[field.key] : null;
        if (!label || used.has(label) || label.dataset.shape !== tunerFieldShape(field)) {
          label = createTunerFieldElement(field);
        }
        used.add(label);
        writeTunerFieldElement(label, field, !force && label === focused);
        desired.push(label);
      });
    });
    const anchor = focused && used.has(focused) ? focused : null;
    Array.from(els.tunerForm.childNodes).forEach(function (node) {
      if (node !== anchor) els.tunerForm.removeChild(node);
    });
    const at = anchor ? desired.indexOf(anchor) : -1;
    desired.forEach(function (node, index) {
      if (anchor && index < at) {
        els.tunerForm.insertBefore(node, anchor);
      } else if (index !== at) {
        els.tunerForm.appendChild(node);
      }
    });
  }

  async function loadTunerConfig() {
    if (!state.activeID) return;
    const ticket = takeTicket("config");
    let config;
    try {
      config = await getJSON("/api/strategies/" + encodeURIComponent(ticket.id) + "/config");
    } catch (err) {
      if (!acceptReply("config", ticket)) return;
      throw err;
    }
    if (!acceptReply("config", ticket)) return;
    if (!config || config.strategy_id !== ticket.id) {
      throw new Error("The config reply names strategy " +
        JSON.stringify(config && config.strategy_id !== undefined ? config.strategy_id : null) +
        " but the request was for " + JSON.stringify(ticket.id));
    }
    const previous = currentTunerConfig();
    state.tuner.config = { strategyID: ticket.id, epoch: ticket.epoch, resp: config };
    const dropped = pruneTunerDraft(previous, config);
    acceptRatchetConfig(ticket.seq, config);
    renderTunerForm(config);
    updateControlsPanel(config);
    if (dropped.length) {
      state.tuner.notice = "The server changed or removed tuner field(s) " + dropped.join(", ") +
        ". Their edits were dropped and will not be applied.";
      updateTunerStatus();
      scheduleSimulate();
    }
    refreshGlobalNotifications().catch(function () {});
  }

  function setControlsMessage(text) {
    if (!els.controlsMessage) return;
    els.controlsMessage.textContent = text || "";
    els.controlsMessage.hidden = !text;
  }

  function ratchetChoice(value) {
    return value === null || value === undefined ? "inherit" : (value ? "on" : "off");
  }

  function acceptRatchetConfig(seq, config) {
    const ratchet = state.tuner.ratchet;
    const reported = ratchetChoice(config.notify_ratchet_triggers);
    const held = ratchet.held;
    if (held) {
      if (seq <= held.afterSeq) return;
      if (reported !== held.value && held.pending.indexOf(reported) !== -1) return;
      ratchet.held = null;
    }
    ratchet.confirmed = reported;
  }

  function updateControlsPanel(config) {
    if (els.pauseToggle) {
      els.pauseToggle.hidden = !config;
      if (config) {
        els.pauseToggle.textContent = config.paused ? "Resume strategy" : "Pause strategy";
        els.pauseToggle.dataset.paused = config.paused ? "1" : "";
      } else {
        els.pauseToggle.disabled = false;
        delete els.pauseToggle.dataset.paused;
      }
    }
    if (els.ratchetNotifySelect) {
      const busy = !!state.tuner.ratchet.ticket;
      els.ratchetNotifySelect.disabled = !config || busy;
      if (!busy) {
        els.ratchetNotifySelect.value = config ? state.tuner.ratchet.confirmed : "inherit";
      }
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

  function writeOutcomeFromReply(resp) {
    if (resp && resp.ok === true) {
      return {
        status: "success",
        message: String(resp.message || ""),
        restartRequired: !!resp.restart_required,
      };
    }
    return { status: "unknown", detail: "the reply did not confirm the write" };
  }

  function writeOutcomeFromError(err) {
    if (err && typeof err.status === "number") {
      return { status: "failed", httpStatus: err.status, message: apiFeedbackError(err), err: err };
    }
    return {
      status: "unknown",
      detail: "the reply was lost or unreadable (" + String((err && err.message) || err || "no reply") + ")",
      err: err,
    };
  }

  function suppressedAuthOutcome(outcome) {
    return outcome.status === "failed" && outcome.httpStatus === 401 && !tokenStampCurrent(outcome.err);
  }

  function describeWriteOutcome(action, strategyID, outcome) {
    const head = action + " for strategy " + strategyID;
    if (outcome.status === "success") {
      return head + " succeeded. " + (outcome.message || "The server accepted the change.");
    }
    if (outcome.status === "failed") {
      return head + " failed: " + outcome.message;
    }
    return head + " has an unknown outcome: " + outcome.detail +
      ". Check the strategy's current config, positions and orders before you try again.";
  }

  function renderLateResults() {
    if (!els.dashboardMessage || !els.dashboardMessageList) return;
    els.dashboardMessageList.replaceChildren.apply(
      els.dashboardMessageList,
      state.lateResults.map(function (entry) {
        const item = document.createElement("li");
        item.className = "dashboard-message-item" + (entry.kind ? " " + entry.kind : "");
        item.textContent = entry.text;
        return item;
      })
    );
    els.dashboardMessage.hidden = !state.lateResults.length;
  }

  function pushLateResult(kind, text) {
    state.lateResults.push({ kind: kind, text: text + " This result arrived after the selection changed." });
    renderLateResults();
  }

  function reportLateResult(action, strategyID, outcome) {
    if (suppressedAuthOutcome(outcome)) return;
    if (outcome.status === "failed" && outcome.httpStatus === 401) {
      handleRefreshError(outcome.err);
    }
    pushLateResult(outcome.status === "success" ? "success" : "error", describeWriteOutcome(action, strategyID, outcome));
  }

  function reportLateNotSent(action, strategyID, reason) {
    pushLateResult("error", action + " for strategy " + strategyID + " was not sent: " + reason + ".");
  }

  function reportLateFailure(action, strategyID, err, sent) {
    if (sent) {
      reportLateResult(action, strategyID, writeOutcomeFromError(err));
      return;
    }
    const outcome = writeOutcomeFromError(err);
    if (suppressedAuthOutcome(outcome)) return;
    if (outcome.status === "failed" && outcome.httpStatus === 401) {
      handleRefreshError(err);
    }
    reportLateNotSent(action, strategyID, outcome.status === "failed" ? outcome.message : outcome.detail);
  }

  function selectionTicket() {
    return { id: state.activeID, epoch: state.selectionEpoch };
  }

  async function togglePause() {
    const config = currentTunerConfig();
    if (!state.activeID || !els.pauseToggle || !config) return;
    const next = !config.paused;
    const action = next ? "Pause" : "Resume";
    const ticket = takeTicket("pause");
    els.pauseToggle.disabled = true;
    let outcome;
    try {
      outcome = writeOutcomeFromReply(await postJSON(
        "/api/strategies/" + encodeURIComponent(ticket.id) + "/pause",
        { paused: next }
      ));
    } catch (err) {
      outcome = writeOutcomeFromError(err);
    }
    if (!acceptReply("pause", ticket)) {
      reportLateResult(action, ticket.id, outcome);
      return;
    }
    try {
      if (outcome.status !== "success") {
        if (!suppressedAuthOutcome(outcome)) {
          setControlsMessage(outcome.status === "failed"
            ? action + " failed: " + outcome.message
            : describeWriteOutcome(action, ticket.id, outcome));
        }
        return;
      }
      setControlsMessage(outcome.message);
      try {
        await loadTunerConfig();
      } catch (err) {
        if (ticketSelected(ticket)) handleRefreshError(err);
      }
      if (!ticketSelected(ticket)) {
        reportLateResult(action, ticket.id, outcome);
        return;
      }
      await refreshAll();
      if (!ticketSelected(ticket)) {
        reportLateResult(action, ticket.id, outcome);
      }
    } finally {
      if (ticketSelected(ticket)) {
        els.pauseToggle.disabled = false;
      }
    }
  }

  async function changeStrategyRatchetNotify() {
    const select = els.ratchetNotifySelect;
    if (!state.activeID || !select || !currentTunerConfig() || state.tuner.ratchet.ticket) return;
    const choice = select.value;
    const ticket = takeTicket("notify");
    state.tuner.ratchet.ticket = ticket;
    select.disabled = true;
    let outcome;
    try {
      outcome = writeOutcomeFromReply(await postJSON(
        "/api/strategies/" + encodeURIComponent(ticket.id) + "/notifications",
        { notify_ratchet_triggers: triStateToValue(choice, "inherit") }
      ));
    } catch (err) {
      outcome = writeOutcomeFromError(err);
    }
    if (!acceptReply("notify", ticket)) {
      reportLateResult("Ratchet alert change", ticket.id, outcome);
      return;
    }
    const ratchet = state.tuner.ratchet;
    ratchet.ticket = null;
    if (outcome.status === "success") {
      if (ratchet.held || choice !== ratchet.confirmed) {
        const stale = ratchet.held ? ratchet.held.pending.concat([ratchet.held.value]) : [ratchet.confirmed];
        ratchet.held = {
          value: choice,
          pending: stale.filter(function (value, index) {
            return value !== choice && stale.indexOf(value) === index;
          }),
          afterSeq: state.lanes.config.issued,
        };
      }
      ratchet.confirmed = choice;
      setControlsMessage(outcome.message);
    } else if (!suppressedAuthOutcome(outcome)) {
      setControlsMessage(outcome.status === "failed"
        ? "Ratchet alert change failed: " + outcome.message
        : describeWriteOutcome("Ratchet alert change", ticket.id, outcome));
    }
    updateControlsPanel(currentTunerConfig());
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
    return !!strat && strat.type === "perps" && strat.platform === "hyperliquid" && strat.partition === "live";
  }

  function strategySupportsPaperClose(strat) {
    return !!strat && !!strat.partition && strat.partition !== "live";
  }

  function updateTradePanel(status) {
    if (!els.tradePanel) return;
    const strat = activeStrategyMeta();
    const manual = strategySupportsManualActions(strat);
    const forceClose = strategySupportsForceClose(strat);
    const hasPosition = !!(status && status.positions && Object.keys(status.positions).length);
    const hasOptionLeg = !!(status && status.option_positions && Object.keys(status.option_positions).length);
    const paperClose = strategySupportsPaperClose(strat) && (hasPosition || hasOptionLeg);
    els.tradePanel.hidden = !(manual || forceClose || paperClose);
    if (els.tradePaperCloseAll) {
      els.tradePaperCloseAll.hidden = !paperClose;
    }
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
      els.tradePositionForm.hidden = !hasPosition || !(manual || forceClose);
    }
    const slField = document.getElementById("trade-sl-field");
    if (slField) {
      slField.hidden = !manual;
    }
  }

  async function confirmedStrategyAction(spec) {
    const ticket = spec.ticket;
    spec.setMessage("");
    let confirm;
    try {
      confirm = await postJSON("/api/confirm", {
        action: spec.action,
        strategy_id: ticket.id,
        params: spec.params,
      });
    } catch (err) {
      if (!ticketSelected(ticket)) {
        reportLateFailure(spec.label, ticket.id, err, false);
        return;
      }
      spec.setMessage("Confirm failed: " + err.message);
      return;
    }
    if (!ticketSelected(ticket)) {
      reportLateNotSent(spec.label, ticket.id, "the selection changed before its confirmation dialog opened");
      return;
    }
    const proceed = await showTradeConfirmDialog(confirm);
    if (!proceed) {
      if (ticketSelected(ticket)) spec.setMessage("Cancelled.");
      return;
    }
    let resp = null;
    let failure = null;
    try {
      resp = await postJSON(spec.url, { nonce: confirm.nonce, params: spec.params });
    } catch (err) {
      failure = err;
    }
    if (!ticketSelected(ticket)) {
      reportLateResult(spec.label, ticket.id, failure ? writeOutcomeFromError(failure) : writeOutcomeFromReply(resp));
      return;
    }
    const failureOutcome = failure ? writeOutcomeFromError(failure) : null;
    if (failureOutcome && failureOutcome.status !== "unknown") {
      spec.setMessage(spec.action + " failed: " + failure.message);
      return;
    }
    spec.setMessage(failureOutcome
      ? describeWriteOutcome(spec.label, ticket.id, failureOutcome)
      : (resp && resp.message) || spec.doneText);
    await refreshAll();
    if (!ticketSelected(ticket)) {
      reportLateResult(spec.label, ticket.id, failureOutcome || writeOutcomeFromReply(resp));
    }
  }

  function confirmTradeAction(action, params) {
    if (!state.activeID) return Promise.resolve();
    const ticket = selectionTicket();
    return confirmedStrategyAction({
      ticket: ticket,
      action: action,
      label: "Trade action " + action,
      params: params,
      url: "/api/strategies/" + encodeURIComponent(ticket.id) + "/" + action,
      setMessage: setTradeMessage,
      doneText: "Submitted.",
    });
  }

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

  function setStructuralMessage(text, el) {
    const target = el || els.structuralMessage;
    if (!target) return;
    target.textContent = text || "";
    target.hidden = !text;
  }

  function updateStructuralPanel() {
    if (!els.structuralPanel) return;
    const strat = activeStrategyMeta();
    els.structuralPanel.hidden = !strat;
    if (!strat) return;
    const gateable = strat.type === "perps" || strat.type === "futures";
    if (els.paperToLiveButton) els.paperToLiveButton.hidden = !gateable;
    if (els.applyRegimeGateButton) els.applyRegimeGateButton.hidden = !gateable;
  }

  async function confirmStructuralAction(action, strategyID, params, url, messageEl) {
    if (strategyID) {
      if (strategyID !== state.activeID) return;
      return confirmedStrategyAction({
        ticket: selectionTicket(),
        action: action,
        label: "Structural action " + action,
        params: params,
        url: url,
        setMessage: function (text) { setStructuralMessage(text, messageEl); },
        doneText: "Done.",
      });
    }
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
    const openParamsText = (els.addStratOpenParams && els.addStratOpenParams.value || "").trim();
    if (openParamsText) {
      let parsed;
      try {
        parsed = JSON.parse(openParamsText);
      } catch (err) {
        setStructuralMessage("Open params must be a JSON object: " + err.message, els.addStratMessage);
        return Promise.resolve();
      }
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        setStructuralMessage("Open params must be a JSON object.", els.addStratMessage);
        return Promise.resolve();
      }
      params.open_params = parsed;
    }
    if (els.addStratRestart && els.addStratRestart.checked) {
      params.restart = true;
    }
    return confirmStructuralAction("add-strategy", "", params, "/api/config/add-strategy", els.addStratMessage);
  }

  async function structuralAddQuoteOwner() {
    const file = els.addQuoteFile && els.addQuoteFile.files && els.addQuoteFile.files[0];
    if (!file) {
      setStructuralMessage("Choose a quote_owner JSON file.", els.addQuoteMessage);
      return;
    }
    if (file.size > 1048576) {
      setStructuralMessage("The quote_owner file exceeds 1 MiB.", els.addQuoteMessage);
      return;
    }
    let text;
    try {
      text = await file.text();
    } catch (err) {
      setStructuralMessage("The quote_owner file could not be read: " + err.message, els.addQuoteMessage);
      return;
    }
    const params = { fragment_text: text };
    if (els.addQuoteRestart && els.addQuoteRestart.checked) {
      params.restart = true;
    }
    return confirmStructuralAction("add-quote-owner", "", params, "/api/config/add-quote-owner", els.addQuoteMessage);
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

  function buildSimulateOverrides(config, draft) {
    const overrides = {};
    Object.keys(draft).forEach(function (key) {
      overrides[key] = draft[key];
    });
    if (config && config.open_strategy && config.open_strategy.params) {
      const paramOverrides = {};
      Object.keys(overrides).forEach(function (key) {
        if (key.indexOf("open_strategy.params.") === 0) {
          paramOverrides[key.slice("open_strategy.params.".length)] = overrides[key];
          delete overrides[key];
        }
      });
      if (Object.keys(paramOverrides).length) {
        overrides.open_strategy = {
          name: config.open_strategy.name,
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

  function refreshChartAndTrades() {
    return Promise.all([refreshChart(), refreshTradeHistory()]);
  }

  async function runSimulate() {
    if (!state.activeID) return;
    if (!tunerHasOverrides()) {
      invalidatePreview();
      state.tuner.previewActive = false;
      state.tuner.liveMarkers = [];
      state.tuner.simulatedMarkers = [];
      state.tuner.liveRefusal = "";
      state.tuner.simulatedRefusal = "";
      updateTunerStatus();
      await refreshChartAndTrades();
      return;
    }
    const config = currentTunerConfig();
    if (!config) return;
    state.tuner.previewSeq += 1;
    const ticket = { id: state.activeID, epoch: state.selectionEpoch, seq: state.tuner.previewSeq };
    state.tuner.loading = true;
    updateTunerStatus();
    try {
      const resp = await postJSON(
        "/api/strategies/" + encodeURIComponent(ticket.id) + "/simulate",
        { overrides: buildSimulateOverrides(config, state.tuner.overrides), limit: 400 }
      );
      if (!previewTicketCurrent(ticket)) return;
      state.tuner.liveMarkers = resp.live_markers || [];
      state.tuner.simulatedMarkers = resp.simulated_markers || [];
      state.tuner.liveRefusal = resp.live_refusal || "";
      state.tuner.simulatedRefusal = resp.simulated_refusal || "";
      state.tuner.previewActive = true;
      state.tuner.previewFor = ticket.id;
      await refreshChartAndTrades();
    } catch (err) {
      if (!previewTicketCurrent(ticket)) return;
      throw err;
    } finally {
      if (previewTicketCurrent(ticket)) {
        state.tuner.loading = false;
        updateTunerStatus();
      }
    }
  }

  function onTunerFieldChange(event) {
    const target = event.target;
    const key = target.dataset.key;
    if (!key) return;
    const config = currentTunerConfig();
    if (!config) return;
    const base = findTunerField(config, key);
    const label = target.closest("label.tuner-field");
    if (!base || !label || !els.tunerForm.contains(label) || label.dataset.shape !== tunerFieldShape(base)) {
      return;
    }
    let value;
    if (target.type === "checkbox") {
      value = target.checked;
    } else if (target.type === "number") {
      value = target.value === "" ? null : Number(target.value);
    } else {
      value = target.value;
    }
    const baseline = base.value;
    if (value === baseline || (value === null && (baseline === null || baseline === undefined))) {
      delete state.tuner.overrides[key];
    } else {
      state.tuner.overrides[key] = value;
    }
    state.tuner.notice = "";
    updateTunerStatus();
    scheduleSimulate();
  }

  function resetTunerToLive() {
    const config = currentTunerConfig();
    if (!config) return;
    state.tuner.overrides = {};
    state.tuner.notice = "";
    renderTunerForm(config, { force: true });
    invalidatePreview();
    state.tuner.previewActive = false;
    state.tuner.liveMarkers = [];
    state.tuner.simulatedMarkers = [];
    state.tuner.liveRefusal = "";
    state.tuner.simulatedRefusal = "";
    updateTunerStatus();
    refreshChartAndTrades().catch(handleRefreshError);
  }

  function tunerApplyRequiresRestartClient(config, draft) {
    const keys = Object.keys(draft);
    if (config && config.has_open_position) {
      if (keys.indexOf("direction") >= 0 || keys.indexOf("invert_signal") >= 0 || keys.indexOf("leverage") >= 0) {
        return true;
      }
    }
    return keys.indexOf("htf_filter") >= 0;
  }

  function tunerApplyConfirmText(record) {
    const edits = Object.keys(record.draft).sort().map(function (key) {
      return key + " = " + JSON.stringify(record.draft[key]);
    });
    const sentence = record.restart
      ? "This writes tuned parameters to the live config file. Restart go-trader afterward — hot reload cannot apply most tuner fields."
      : "This writes tuned parameters to the live config file. Send SIGHUP to reload when ready.";
    return "Strategy " + record.id + ": " + edits.join("; ") + ". " + sentence;
  }

  function requestTunerApply() {
    const config = currentTunerConfig();
    if (!config || !tunerHasOverrides() || state.tuner.applyTicket) return;
    const draft = JSON.parse(JSON.stringify(state.tuner.overrides));
    const record = deepFreeze({
      id: state.activeID,
      epoch: state.selectionEpoch,
      draft: draft,
      overrides: buildSimulateOverrides(config, draft),
      restart: tunerApplyRequiresRestartClient(config, draft),
    });
    state.tuner.pendingApply = record;
    state.tuner.notice = "";
    updateTunerStatus();
    const text = tunerApplyConfirmText(record);
    if (els.tunerConfirmText) {
      els.tunerConfirmText.textContent = text;
    }
    if (els.tunerConfirmDialog && typeof els.tunerConfirmDialog.showModal === "function") {
      els.tunerConfirmDialog.returnValue = "";
      els.tunerConfirmDialog.showModal();
    } else if (window.confirm(text)) {
      submitConfirmedApply().catch(handleRefreshError);
    } else {
      state.tuner.pendingApply = null;
    }
  }

  function submitConfirmedApply() {
    const record = state.tuner.pendingApply;
    state.tuner.pendingApply = null;
    if (!record) return Promise.resolve();
    const config = currentTunerConfig();
    const draft = state.tuner.overrides;
    const unchanged = !!config &&
      !state.tuner.applyTicket &&
      record.id === state.activeID &&
      record.epoch === state.selectionEpoch &&
      stableJSON(draft) === stableJSON(record.draft) &&
      stableJSON(buildSimulateOverrides(config, draft)) === stableJSON(record.overrides) &&
      tunerApplyRequiresRestartClient(config, draft) === record.restart;
    if (!unchanged) {
      state.tuner.notice = "The selection, the edits or the restart warning changed. Nothing was sent. Review and confirm again.";
      updateTunerStatus();
      return Promise.resolve();
    }
    return applyTunerConfig(record);
  }

  function releaseApplyBusy(ticket) {
    if (state.tuner.applyTicket !== ticket) return;
    state.tuner.applyTicket = null;
    if (els.tunerApply) {
      els.tunerApply.disabled = false;
    }
  }

  async function applyTunerConfig(record) {
    const ticket = takeTicket("apply");
    state.tuner.applyTicket = ticket;
    if (els.tunerApply) {
      els.tunerApply.disabled = true;
    }
    let outcome;
    try {
      outcome = writeOutcomeFromReply(await postJSON(
        "/api/strategies/" + encodeURIComponent(record.id) + "/config",
        { overrides: record.overrides }
      ));
    } catch (err) {
      outcome = writeOutcomeFromError(err);
    }
    releaseApplyBusy(ticket);
    if (!acceptReply("apply", ticket)) {
      reportLateResult("Apply", record.id, outcome);
      return;
    }
    if (outcome.status !== "success") {
      if (!suppressedAuthOutcome(outcome)) {
        state.tuner.notice = describeWriteOutcome("Apply", record.id, outcome);
        updateTunerStatus();
      }
      if (outcome.err) throw outcome.err;
      return;
    }
    state.tuner.notice = describeWriteOutcome("Apply", record.id, outcome);
    Object.keys(record.draft).forEach(function (key) {
      if (hasOwn(state.tuner.overrides, key) && sameValue(state.tuner.overrides[key], record.draft[key])) {
        delete state.tuner.overrides[key];
      }
    });
    if (tunerHasOverrides()) {
      scheduleSimulate();
    } else {
      invalidatePreview();
      state.tuner.previewActive = false;
      state.tuner.liveMarkers = [];
      state.tuner.simulatedMarkers = [];
      state.tuner.liveRefusal = "";
      state.tuner.simulatedRefusal = "";
      if (state.tuner.simulateTimer) {
        clearTimeout(state.tuner.simulateTimer);
        state.tuner.simulateTimer = 0;
      }
    }
    updateTunerStatus();
    const config = currentTunerConfig();
    if (config) {
      renderTunerForm(config);
    }
    retireLane("config");
    let refreshFailed = false;
    try {
      await loadTunerConfig();
    } catch (err) {
      refreshFailed = true;
      if (ticketSelected(ticket)) handleRefreshError(err);
    }
    if (!ticketSelected(ticket)) {
      reportLateResult("Apply", record.id, outcome);
      return;
    }
    await refreshAll();
    if (!ticketSelected(ticket)) {
      reportLateResult("Apply", record.id, outcome);
      return;
    }
    if (!refreshFailed) {
      els.statusLabel.textContent = outcome.restartRequired ? "Apply OK — restart" : "Apply OK";
    }
  }

  function markerText(marker) {
    if (!marker.realized_pnl) return marker.text;
    const pnl = marker.realized_pnl >= 0 ? "+" + fmtMoney(marker.realized_pnl) : fmtMoney(marker.realized_pnl);
    return marker.text + " " + pnl;
  }

  function chartMarkersFromResponse(tradeResp) {
    if (state.tuner.previewActive && state.tuner.previewFor === state.activeID && state.tuner.simulatedMarkers.length) {
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
    return ((tradeResp && tradeResp.markers) || []).map(function (m) {
      return {
        time: m.time,
        position: m.position,
        color: m.color,
        shape: m.shape,
        text: markerText(m),
      };
    });
  }

  const CHART_INITIAL_LIMIT = 400;
  const CHART_TAIL_LIMIT = 50;
  const CHART_PAGE_LIMIT = 300;
  const CHART_GAP_LIMIT = 1000;
  const CHART_OLDER_DEBOUNCE_MS = 300;
  const CHART_UNAVAILABLE_RETRY_S = 10;

  function chartLogic() {
    return (window.GoTraderChart && window.GoTraderChart.logic) || null;
  }

  function resetChartSeries(timeframe) {
    state.chartEpoch += 1;
    state.chartTimeframe = timeframe || "";
    state.chartSeries = null;
    if (state.chartOlder.timer) clearTimeout(state.chartOlder.timer);
    state.chartOlder = { inFlight: false, timer: 0 };
    state.chartBackoff = { until: 0, attempt: 0 };
    setChartNote("");
    if (els.chartSource) els.chartSource.textContent = "";
    if (!timeframe && els.chartTimeframe) {
      els.chartTimeframe.replaceChildren();
      els.chartTimeframe.disabled = true;
    }
  }

  function setChartNote(text) {
    if (!els.chartNote) return;
    els.chartNote.textContent = text || "";
    els.chartNote.hidden = !text;
  }

  function candleTicket(lane) {
    const ticket = takeTicket(lane);
    ticket.chartEpoch = state.chartEpoch;
    ticket.timeframe = state.chartTimeframe;
    return ticket;
  }

  function candleTicketCurrent(ticket) {
    return ticketSelected(ticket) && ticket.chartEpoch === state.chartEpoch && ticket.timeframe === state.chartTimeframe;
  }

  function acceptCandleReply(lane, ticket) {
    return candleTicketCurrent(ticket) && acceptReply(lane, ticket);
  }

  function candleReplyMatches(body, ticket) {
    return !!body && body.strategy_id === ticket.id && (!ticket.timeframe || body.timeframe === ticket.timeframe);
  }

  function currentChartSeries() {
    const s = state.chartSeries;
    if (!s || s.strategyID !== state.activeID || s.selectionEpoch !== state.selectionEpoch || s.chartEpoch !== state.chartEpoch) return null;
    return s;
  }

  function candlesURL(id, params) {
    const q = new URLSearchParams();
    Object.keys(params).forEach(function (key) {
      if (params[key] !== undefined && params[key] !== null && params[key] !== "") q.set(key, String(params[key]));
    });
    return "/api/strategies/" + encodeURIComponent(id) + "/candles?" + q.toString();
  }

  async function fetchCandles(url, etag) {
    const generation = statusToken.generation();
    const headers = Object.assign({}, authHeaders());
    if (etag) headers["If-None-Match"] = etag;
    const res = await fetch(url, { headers: headers, cache: "no-store" });
    const out = {
      status: res.status,
      etag: res.headers.get("ETag") || "",
      retryAfter: Number(res.headers.get("Retry-After")) || 0,
      body: null,
      text: "",
    };
    if (res.status === 401) {
      const err = new Error(await res.text() || res.statusText);
      err.status = 401;
      err.tokenGeneration = generation;
      throw err;
    }
    if (res.status === 304) return out;
    out.text = await res.text();
    try {
      out.body = out.text ? JSON.parse(out.text) : null;
    } catch (_err) {
      out.body = null;
    }
    return out;
  }

  function chartHasBars() {
    return !!(state.chart && state.chart.lastBarTime() !== null);
  }

  function candleRefusal(r) {
    if (r.status === 200 || r.status === 304) return "";
    const body = r.body || {};
    const code = body.code || "";
    const detail = body.error || r.text || ("HTTP " + r.status);
    const logic = chartLogic();
    if (r.status === 429 || r.status === 503) {
      const retry = r.status === 429 ? r.retryAfter : (r.retryAfter || CHART_UNAVAILABLE_RETRY_S);
      const delay = logic ? logic.backoffDelay(retry, state.chartBackoff.attempt) : retry * 1000;
      state.chartBackoff = { until: Date.now() + delay, attempt: state.chartBackoff.attempt + 1 };
      setChartNote((r.status === 429 ? "Chart reads are at their local limit" : "Candle source unavailable (" + code + "): " + detail) +
        ". The chart keeps its bars and retries in " + Math.ceil(delay / 1000) + " s.");
      return code || String(r.status);
    }
    if (r.status === 409 && code === "strategy_changed") {
      state.chartSeries = null;
      setChartNote("The strategy changed during the read; the chart reloads.");
      return code;
    }
    if (r.status === 409 && code === "cursor_expired") return code;
    if (r.status === 400 && (code === "timeframe_unsupported" || code === "timeframe_invalid") && state.chartTimeframe) {
      resetChartSeries("");
      setChartNote("Timeframe no longer available (" + detail + "); the chart went back to the configured timeframe.");
      return code;
    }
    if (chartHasBars()) {
      setChartNote("The candle request failed: " + detail);
    } else {
      showChartFailure("The candle request failed: " + detail);
    }
    return code || String(r.status);
  }

  function describeChartSource(body) {
    const parts = [];
    if (body.source_class === "synthetic") {
      parts.push("Trade-built bars (volume and indicators hidden)");
    } else if (body.origin) {
      parts.push(body.origin === "published" ? "Feed bars" : "Archive bars");
    }
    if (body.source) parts.push(body.source);
    if (body.freshness === "stale") parts.push("stale" + (body.freshness_detail ? ": " + body.freshness_detail : ""));
    return parts.join(" · ");
  }

  function renderTimeframeSelect(caps, current) {
    const select = els.chartTimeframe;
    if (!select || !caps) return;
    const list = caps.timeframes || [];
    const key = list.map(function (t) { return t.timeframe + ":" + t.origin; }).join(",") + "|" + current;
    if (select.dataset.key === key) return;
    select.dataset.key = key;
    select.replaceChildren();
    list.forEach(function (t) {
      const opt = document.createElement("option");
      opt.value = t.timeframe;
      opt.textContent = t.timeframe + (t.timeframe === caps.default_timeframe ? " (configured)" : "") + " · " + t.origin;
      select.append(opt);
    });
    select.value = current;
    select.disabled = list.length < 2;
  }

  function afterChartDraw(body) {
    state.chartData = { strategyID: body.strategy_id, epoch: state.selectionEpoch, resp: body };
    drawChartMarkers();
    drawPositionLines();
    hideChartFailure();
    els.empty.style.display = chartHasBars() ? "none" : "flex";
    if (els.chartSource) els.chartSource.textContent = describeChartSource(body);
    if (body.market_availability && body.source_class === "synthetic") {
      setChartNote("Market bars unavailable (" + body.market_availability.code + "): " + body.market_availability.detail);
    } else if (body.market_availability && !chartHasBars()) {
      setChartNote("No bars (" + body.market_availability.code + "): " + body.market_availability.detail);
    }
  }

  function drawChartMarkers() {
    const chart = state.chart;
    const series = currentChartSeries();
    if (!chart || !series) return false;
    const history = state.tradeHistory && state.tradeHistory.strategyID === state.activeID
      ? state.tradeHistory.resp
      : null;
    const logic = chartLogic();
    const markers = chartMarkersFromResponse(history);
    const mapped = logic ? logic.mapMarkersToBars(markers, chart.barTimes(), series.barSeconds) : markers;
    return chart.setMarkers(mapped);
  }

  function currentInput(record) {
    return !!record && record.strategyID === state.activeID && record.epoch === state.selectionEpoch;
  }

  function drawPositionLines() {
    const chart = state.chart;
    if (!chart) return true;
    let lines = [];
    if (currentInput(state.chartData) && currentInput(state.statusData)) {
      lines = bookPositionLines(activeStrategy(), state.statusData.resp, state.chartData.resp).lines;
    }
    return chart.setPriceLines("position", lines);
  }

  function newChartSeries(ticket, r) {
    const body = r.body;
    return {
      strategyID: ticket.id,
      selectionEpoch: ticket.epoch,
      chartEpoch: ticket.chartEpoch,
      timeframe: body.timeframe,
      token: body.series_token,
      sourceClass: body.source_class,
      barSeconds: body.bar_seconds,
      older: (body.page && body.page.older) || "",
      boundary: (body.page && body.page.boundary) || "",
      etag: r.etag,
      resp: body,
    };
  }

  async function loadChartSeries(notice) {
    const chart = state.chart;
    const ticket = candleTicket("candles");
    const params = { limit: CHART_INITIAL_LIMIT, timeframe: ticket.timeframe };
    let r;
    try {
      r = await fetchCandles(candlesURL(ticket.id, params), "");
    } catch (err) {
      if (!acceptCandleReply("candles", ticket)) return;
      state.chartData = null;
      drawPositionLines();
      if (err.status === 401) throw err;
      showChartFailure("The candle request failed: " + (err.message || String(err)));
      return;
    }
    if (!acceptCandleReply("candles", ticket)) return;
    if (candleRefusal(r)) {
      state.chartData = null;
      drawPositionLines();
      return;
    }
    if (!candleReplyMatches(r.body, ticket)) return;
    state.chartBackoff = { until: 0, attempt: 0 };
    const series = newChartSeries(ticket, r);
    state.chartSeries = series;
    renderTimeframeSelect(r.body.capabilities, r.body.timeframe);
    if (!chart.setSeries({ candles: r.body.candles, source: r.body.source, sourceClass: r.body.source_class, formingTime: r.body.forming_time }, false)) return;
    setChartNote(notice || "");
    afterChartDraw(r.body);
  }

  async function recoverChartGap(series, last, tailFirst) {
    const ticket = candleTicket("candles");
    let r;
    try {
      r = await fetchCandles(candlesURL(ticket.id, { timeframe: series.timeframe, from: last, limit: CHART_GAP_LIMIT }), "");
    } catch (err) {
      if (err.status === 401) throw err;
      if (acceptCandleReply("candles", ticket)) setChartNote("The gap refresh failed; the chart keeps its bars.");
      return;
    }
    if (!acceptCandleReply("candles", ticket) || currentChartSeries() !== series) return;
    if (candleRefusal(r)) return;
    if (!candleReplyMatches(r.body, ticket)) return;
    if (r.body.series_token !== series.token) {
      state.chartSeries = null;
      return;
    }
    const logic = chartLogic();
    if (!logic || logic.gapRecoveryOutcome(last, r.body.candles, tailFirst, series.barSeconds, CHART_GAP_LIMIT) === "reload") {
      resetChartSeries(series.timeframe);
      await loadChartSeries("The chart could not join its bars to the newest bars; it reloaded from the newest bars.");
      return;
    }
    state.chart.applyTail(r.body.candles || [], r.body.forming_time);
    afterChartDraw(r.body);
  }

  async function pollChartTail(series) {
    const chart = state.chart;
    const ticket = candleTicket("candles");
    let r;
    try {
      r = await fetchCandles(candlesURL(ticket.id, { timeframe: series.timeframe, limit: CHART_TAIL_LIMIT }), series.etag);
    } catch (err) {
      if (err.status === 401) throw err;
      if (acceptCandleReply("candles", ticket)) setChartNote("The candle refresh failed; the chart keeps its bars.");
      return;
    }
    if (!acceptCandleReply("candles", ticket) || currentChartSeries() !== series) return;
    if (r.status === 304) {
      state.chartBackoff = { until: 0, attempt: 0 };
      return;
    }
    if (candleRefusal(r)) return;
    if (!candleReplyMatches(r.body, ticket)) return;
    state.chartBackoff = { until: 0, attempt: 0 };
    const body = r.body;
    renderTimeframeSelect(body.capabilities, body.timeframe);
    if (body.series_token !== series.token || body.source_class !== series.sourceClass) {
      const next = newChartSeries(ticket, r);
      state.chartSeries = next;
      if (!chart.setSeries({ candles: body.candles, source: body.source, sourceClass: body.source_class, formingTime: body.forming_time }, true)) return;
      setChartNote("The chart source changed to " + describeChartSource(body) + "; older pages restart from here.");
      afterChartDraw(body);
      return;
    }
    series.etag = r.etag;
    series.resp = body;
    const last = chart.lastBarTime();
    const fresh = (body.candles || []).filter(function (c) {
      return last === null || Number(c.time) >= last;
    });
    const logic = chartLogic();
    const tail = fresh.length ? fresh : body.candles;
    if (last !== null && logic && logic.detectGap(last, tail, series.barSeconds)) {
      await recoverChartGap(series, last, Number(tail[0].time));
      return;
    }
    chart.applyTail(fresh, body.forming_time);
    afterChartDraw(body);
  }

  function onChartNeedOlder() {
    if (state.chartOlder.timer || state.chartOlder.inFlight) return;
    state.chartOlder.timer = setTimeout(function () {
      state.chartOlder.timer = 0;
      loadOlderPage().catch(handleRefreshError);
    }, CHART_OLDER_DEBOUNCE_MS);
  }

  async function loadOlderPage() {
    const series = currentChartSeries();
    if (!series || !series.older || series.boundary || state.chartOlder.inFlight) return;
    if (Date.now() < state.chartBackoff.until) return;
    const older = state.chartOlder;
    older.inFlight = true;
    const ticket = candleTicket("older");
    let r;
    try {
      r = await fetchCandles(candlesURL(ticket.id, { timeframe: series.timeframe, limit: CHART_PAGE_LIMIT, before: series.older }), "");
    } catch (err) {
      older.inFlight = false;
      if (err.status === 401) throw err;
      if (candleTicketCurrent(ticket)) setChartNote("The older-page request failed; scroll again to retry.");
      return;
    }
    older.inFlight = false;
    if (!acceptCandleReply("older", ticket) || currentChartSeries() !== series) return;
    const refused = candleRefusal(r);
    if (refused === "cursor_expired") {
      resetChartSeries(series.timeframe);
      await loadChartSeries("The candle history changed; paging restarted from the newest bars.");
      return;
    }
    if (refused) return;
    if (!candleReplyMatches(r.body, ticket)) return;
    const page = r.body.page || {};
    state.chart.prependBars(r.body.candles || []);
    series.older = page.older || "";
    series.boundary = page.boundary || "";
    if (!series.older && !series.boundary) series.boundary = "unsupported";
    if (series.boundary && page.detail) setChartNote("Older history stops here: " + page.detail);
    drawChartMarkers();
  }

  async function refreshChart() {
    if (!state.activeID) return;
    if (!ensureChart()) return;
    if (Date.now() < state.chartBackoff.until) return;
    const series = currentChartSeries();
    if (!series) {
      await loadChartSeries("");
      return;
    }
    await pollChartTail(series);
  }

  function onChartTimeframeChange() {
    const value = els.chartTimeframe.value;
    const series = currentChartSeries();
    if (series && value === series.timeframe) return;
    if (state.chart) state.chart.clear();
    resetChartSeries(value);
    refreshChart().catch(handleRefreshError);
  }

  async function refreshTradeHistory() {
    if (!state.activeID) return;
    const ticket = takeTicket("trades");
    let resp;
    try {
      resp = await getJSON("/api/strategies/" + encodeURIComponent(ticket.id) + "/trades?limit=400");
    } catch (err) {
      if (!acceptReply("trades", ticket)) return;
      throw err;
    }
    if (!acceptReply("trades", ticket)) return;
    state.tradeHistory = { strategyID: ticket.id, resp: resp };
    renderTradeHistory(resp.trades || resp.markers || []);
    els.tradeHistoryBody.dataset.strategyId = ticket.id;
    drawChartMarkers();
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
      const positionID = trade.position_id ? String(trade.position_id) : "";
      const rowClass = (tradeRowClass(trade) + (positionID ? " trade-history-row-link" : " trade-history-row-legacy")).trim();
      const rowAttrs = positionID
        ? " tabindex=\"0\" role=\"link\" data-position-id=\"" + escapeHTML(positionID) + "\" title=\"Open this position's review window\""
        : " title=\"This row has no position id, so it cannot open a review window\"";
      return (
        "<tr class=\"" + escapeHTML(rowClass) + "\"" + rowAttrs + ">" +
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

  function tradeReviewURL(strategyID, positionID) {
    return "/dashboard/trade.html?strategy=" + encodeURIComponent(strategyID) +
      "&position=" + encodeURIComponent(positionID);
  }

  function openTradeReview(row) {
    const stampedID = els.tradeHistoryBody.dataset.strategyId || "";
    if (!row || !stampedID || stampedID !== state.activeID) return;
    const positionID = row.getAttribute("data-position-id");
    if (!positionID) return;
    window.open(tradeReviewURL(stampedID, positionID), "_blank", "noopener");
  }

  function initTradeHistoryActivation() {
    els.tradeHistoryBody.addEventListener("click", function (event) {
      openTradeReview(event.target.closest("tr[data-position-id]"));
    });
    els.tradeHistoryBody.addEventListener("keydown", function (event) {
      if (event.key !== "Enter" && event.key !== " ") return;
      const row = event.target.closest("tr[data-position-id]");
      if (!row) return;
      event.preventDefault();
      openTradeReview(row);
    });
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
    if (cert && cert !== "certified") {
      return dir + " (" + cert.toUpperCase() + " → default-off)";
    }
    return dir + (cert ? " (certified)" : "");
  }

  function regimeSideCell(side) {
    if (!side || !side.enabled) {
      return null;
    }
    const label = (side.window || "-") + "=" + (side.label || "?");
    const req = side.requested_side || "none";
    if (side.hold_reason) {
      return label + " → " + req + " (held: " + side.hold_reason + ")";
    }
    return label + " → " + req;
  }

  async function refreshStatus() {
    if (!state.activeID) return;
    const ticket = takeTicket("status");
    let status;
    try {
      status = await getJSON("/api/strategies/" + encodeURIComponent(ticket.id) + "/status");
    } catch (err) {
      if (!acceptReply("status", ticket)) return;
      state.statusData = null;
      drawPositionLines();
      throw err;
    }
    if (!acceptReply("status", ticket)) return;
    state.statusData = { strategyID: ticket.id, epoch: ticket.epoch, resp: status };
    drawPositionLines();
    renderStatus(status);
  }

  function renderStatus(status) {
    updateRegimeBadge(status.regime);
    updateDivergenceBadge(status.regime_divergence);
    updatePausedBadge(!!status.paused);
    els.statusDot.className = "status-dot ok";
    els.statusLabel.textContent = "Live";
    const drawdownPct = status.risk_state && status.risk_state.current_drawdown_pct;
    const fields = status.pool_budget ? [
      ["Budget", "Shared wallet pool"],
      ["Net PnL", fmtSignedMoney(status.pnl), status.pnl],
      ["PnL %", "-", null],
    ] : [
      ["Cash", fmtMoney(status.cash)],
      ["Initial", fmtMoney(status.initial_capital)],
      ["Value", fmtMoney(status.portfolio_value)],
      ["PnL", fmtSignedMoney(status.pnl), status.pnl],
      ["PnL %", fmtPct(status.pnl_pct), status.pnl_pct],
    ];
    fields.push(
      ["Regime", status.regime || "-"],
      ["Drawdown", fmtPct(drawdownPct), drawdownPct, true],
      ["Leverage", fmtNumber(status.leverage)],
      ["Trades", String(status.lifetime_stats ? status.lifetime_stats.positions_opened || 0 : 0)],
      ["W/L", winLoss(status)],
      ["Win Rate", status.win_rate ? fmtPct(status.win_rate) : "-"],
      ["Sharpe", status.sharpe ? fmtNumber(status.sharpe) : "-"],
    );
    const dirCell = directionCell(status);
    if (dirCell) {
      fields.push(["Direction", dirCell]);
    }
    const sideCell = regimeSideCell(status.regime_side);
    if (sideCell) {
      fields.push(["Regime side", sideCell]);
    }
    if (status.regime_profile && status.regime_profile.active_profile) {
      let profile = status.regime_profile.active_profile;
      if (status.regime_profile.pending_profile) {
        profile += " → " + status.regime_profile.pending_profile + " pending";
      }
      fields.push(["Profile", profile]);
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
      rows.push(positionRow(symbol, pos.side || "long", pos.quantity, pos.avg_cost, pos.stop_loss_trigger_px));
    });
    Object.keys(optionPositions).sort().forEach(function (symbol) {
      rows.push(optionPositionRow(symbol, optionPositions[symbol]));
    });
    els.positions.innerHTML = rows.length ? rows.join("") : '<div class="position-row"><span>Flat</span><span>-</span></div>';
  }

  function positionRow(symbol, side, qty, price, sl) {
    const klass = side === "short" || side === "sell" ? "pos-short" : "pos-long";
    const detail = "Qty " + fmtNumber(qty) + " @ " + fmtMoney(price) + (sl ? " / SL " + fmtMoney(sl) : "");
    return '<div class="position-row"><strong>' + escapeHTML(symbol) + '</strong><span class="' + klass + '">' +
      escapeHTML(side || "-") + '</span><span>' + escapeHTML(detail) + '</span><span>' + positionActionButtons(symbol) + '</span></div>';
  }

  function optionMarkAge(updatedAt) {
    const ms = Date.parse(updatedAt || "");
    if (!Number.isFinite(ms) || ms <= 0) {
      return "";
    }
    const minutes = Math.max(0, Math.round((Date.now() - ms) / 60000));
    if (minutes < 1) {
      return "<1m";
    }
    const hours = Math.floor(minutes / 60);
    if (hours >= 48) {
      return Math.floor(hours / 24) + "d" + (hours % 24) + "h";
    }
    return hours > 0 ? hours + "h" + (minutes % 60) + "m" : minutes + "m";
  }

  function optionPositionRow(symbol, pos) {
    const side = pos.action || "";
    const klass = side === "sell" ? "pos-short" : "pos-long";
    const age = optionMarkAge(pos.mark_updated_at);
    let mark = "mark unknown";
    if (age) {
      mark = "mark " + fmtMoney(pos.current_value_usd) + " " + age + " ago (" + (pos.mark_source || "unknown source") + ")";
    }
    let detail = "Qty " + fmtNumber(pos.quantity) + " @ " + fmtMoney(pos.entry_premium_usd) + " / " + mark;
    if (pos.mark_error) {
      detail += " / " + pos.mark_error;
    }
    return '<div class="position-row"><strong>' + escapeHTML(symbol) + '</strong><span class="' + klass + '">' +
      escapeHTML(side || "-") + '</span><span>' + escapeHTML(detail) + '</span><span>' + positionActionButtons(symbol) + '</span></div>';
  }

  function positionActionButtons(symbol) {
    const strat = activeStrategyMeta();
    const buttons = [];
    if (strategySupportsPaperClose(strat)) {
      buttons.push('<button type="button" class="trade-row-button danger" data-trade-action="paper-close" data-symbol="' +
        escapeHTML(symbol) + '">Paper close</button>');
    }
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
    if (key === "pnl_pct" || key === "win_rate" || key === "sharpe" || key === "trade_count" || key === "pnl") {
      const n = Number(row[key]);
      return Number.isFinite(n) ? n : -Infinity;
    }
    const value = row[key];
    return value === undefined || value === null ? "" : String(value).toLowerCase();
  }

  function sortedOverviewRows() {
    const rows = state.overviewRows.filter(function (row) {
      return state.modeFilter === "all" || row.mode === state.modeFilter;
    });
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
    document.querySelectorAll(".mode-filter-button").forEach(function (button) {
      const active = button.dataset.mode === state.modeFilter;
      button.classList.toggle("active", active);
      button.setAttribute("aria-pressed", active ? "true" : "false");
    });
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
        "<td>" + (row.engine_hold ? '<span title="' + escapeHTML("Held (" + row.engine_hold.reason + "): " + row.engine_hold.text + " — " + row.engine_hold.tracking_url) + '">[held]</span> ' : "") + (row.paused ? '<span title="Paused">⏸</span> ' : "") + escapeHTML(row.id) + "</td>" +
        "<td>" + escapeHTML(row.platform || "-") + "</td>" +
        "<td>" + escapeHTML(row.symbol || "-") + "</td>" +
        "<td>" + escapeHTML(row.mode || "-") + "</td>" +
        "<td>" + escapeHTML(String(row.trade_count || 0)) + "</td>" +
        '<td class="' + pnlClassName + '">' + escapeHTML(row.pool_budget ? "—" : fmtNumber(row.pnl)) + "</td>" +
        '<td class="' + pnlClassName + '">' + escapeHTML(row.pool_budget ? "—" : fmtPct(row.pnl_pct)) + "</td>" +
        "<td>" + escapeHTML(row.win_rate ? fmtPct(row.win_rate) : "-") + "</td>" +
        "<td>" + escapeHTML(row.sharpe ? fmtNumber(row.sharpe) : "-") + "</td>" +
        "<td>" + escapeHTML(row.regime || "-") + "</td>" +
        "<td>" + escapeHTML(row.direction || "-") + "</td>" +
        "<td>" + escapeHTML(row.close_strategy || "-") + "</td>" +
        "</tr>";
    }).join("");
    updateSortButtons();
  }

  async function refreshOverview() {
    const read = await getJSONForPartition("/api/strategies/overview");
    if (!partitionStillSelected(read.partition)) return;
    state.overviewRows = read.data.strategies || [];
    renderOverviewTable();
    els.statusDot.className = "status-dot ok";
    els.statusLabel.textContent = "Live";
    els.statusGrid.innerHTML = "<dt>Strategies</dt><dd>" + escapeHTML(String(state.overviewRows.length)) + "</dd>";
    els.positions.innerHTML = '<div class="position-row"><span>Table view</span><span>Select a row for detail</span></div>';
  }


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
      state.partitions = status.partitions || [];
      renderPartitionSelect();
      const rows = [];
      const byScope = status.portfolio_risk_by_scope || {};
      let scopeKeys = Object.keys(byScope).sort().filter(partitionSelected);
      // The untagged single-value field is the legacy scope's, so it may be
      // read only when the operator asked for every partition.
      if (!scopeKeys.length && state.partitionFilter === "all") {
        byScope[""] = status.portfolio_risk || {};
        scopeKeys = [""];
      }
      if (!scopeKeys.length) {
        rows.push('<div class="panel-row panel-muted">No portfolio risk reading for this partition yet</div>');
      }
      scopeKeys.forEach(function (scope) {
        const pr = byScope[scope] || {};
        const tag = scope ? " [" + scope + "]" : "";
        if (pr.kill_switch_active) {
          rows.push('<div class="panel-row risk-alert">🛑 Kill switch ACTIVE' + escapeHTML(tag) + " (drawdown " +
            escapeHTML(fmtPct(pr.current_drawdown_pct)) + ")</div>");
        } else {
          rows.push('<div class="panel-row">Kill switch' + escapeHTML(tag) + ": off (drawdown " +
            escapeHTML(fmtPct(pr.current_drawdown_pct)) + ")</div>");
        }
      });
      const riskHeaderRows = rows.length;
      const now = Date.now();
      Object.keys(status.strategies || {}).sort().forEach(function (id) {
        const entry = status.strategies[id] || {};
        if (!partitionRowVisible(entry.partition)) return;
        const rs = entry.risk_state || {};
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
      if (rows.length === riskHeaderRows) {
        rows.push('<div class="panel-row panel-muted">No active circuit breakers</div>');
      }
      els.riskContent.innerHTML = rows.join("");
    } catch (_err) {
      panelFallback(els.riskContent, "-");
    }
  }

  function regimeReadinessLabel(snap) {
    if (!snap || !Number.isInteger(snap.need) || snap.need <= 0) return "unavailable";
    return snap.ready ? snap.regime : "warming (" + snap.bars + "/" + snap.need + ")";
  }

  function regimeIdentityLabel(platform, assetClass) {
    if (assetClass) return platform + ":" + assetClass;
    if (platform === "robinhood") return platform + ":unclassified";
    return platform || "";
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
        const identity = regimeIdentityLabel(entry.platform, entry.asset_class);
        const title = [entry.symbol, entry.timeframe].filter(Boolean).join(" ") +
          (identity ? " (" + identity + ")" : "");
        const windows = entry.windows || {};
        const windowRows = Object.keys(windows).sort().map(function (name) {
          const win = windows[name] || {};
          const label = win.status || "unavailable";
          const views = [];
          if (win.adx3) views.push("adx3: " + regimeReadinessLabel(win.adx3));
          if (win.composite7) views.push("c7: " + regimeReadinessLabel(win.composite7));
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
        const classed = row.asset_class || row.platform === "robinhood";
        const scope = [row.symbol, row.timeframe, row.window].filter(Boolean).join(" ") +
          (classed ? " (" + regimeIdentityLabel(row.platform, row.asset_class) + ")" : "");
        return '<div class="panel-row"><span class="panel-muted">' + escapeHTML(when) + "</span> " +
          escapeHTML(scope) + ": " + escapeHTML(humanizeRegimeLabel(row.old_label || "-")) +
          " → <strong>" + escapeHTML(humanizeRegimeLabel(row.new_label || "-")) + "</strong>" +
          (row.alerted_at ? " 🔔" : "") + "</div>";
      }).join("");
    } catch (_err) {
      panelFallback(els.transitionsContent, "-");
    }
  }


  function opsPnlClass(v) {
    return v > 0 ? "pnl-pos" : v < 0 ? "pnl-neg" : "";
  }

  async function refreshLeaderboardPanel() {
    if (!els.leaderboardBody) return;
    try {
      const read = await getJSONForPartition("/api/leaderboard");
      if (!partitionStillSelected(read.partition)) return;
      const entries = read.data.entries || [];
      els.leaderboardEmpty.textContent = "No strategies to rank";
      els.leaderboardEmpty.hidden = entries.length > 0;
      els.leaderboardBody.innerHTML = entries.map(function (e, i) {
        return "<tr>" +
          "<td>" + (i + 1) + "</td>" +
          "<td>" + escapeHTML(e.id) + "</td>" +
          '<td class="' + opsPnlClass(e.pnl_pct) + '">' + escapeHTML(e.pool_budget ? "—" : fmtPct(e.pnl_pct)) + "</td>" +
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
    return v === null || v === undefined ? "…" : fmtPct(v);
  }

  async function refreshDiagnosticsPanel() {
    if (!els.diagnosticsBody) return;
    try {
      const read = await getJSONForPartition("/api/diagnostics?limit=25");
      if (!partitionStillSelected(read.partition)) return;
      const rows = read.data.rows || [];
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
      const read = await getJSONForPartition("/api/cashflow");
      if (!partitionStillSelected(read.partition)) return;
      const resp = read.data;
      const rows = [];
      if (resp.available === false) {
        panelFallback(els.cashflowContent, resp.unavailable || "cash flow is owned by the live partition");
        return;
      }
      if (!resp.alarm_enabled) {
        rows.push('<div class="panel-row risk-alert">Journal drift alarm operator-disabled</div>');
      }
      (resp.wallets || []).forEach(function (wallet) {
        const label = wallet.platform + "/" + wallet.account;
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
      const byScope = resp.correlation_by_scope || {};
      let scopeKeys = Object.keys(byScope).sort().filter(partitionSelected);
      // The untagged snapshot belongs to the legacy scope, so a selected
      // partition with no snapshot of its own reports none rather than
      // borrowing another partition's gross exposure and warnings.
      if (!scopeKeys.length && resp.correlation && state.partitionFilter === "all") {
        byScope[""] = resp.correlation;
        scopeKeys = [""];
      }
      if (!scopeKeys.length) {
        panelFallback(els.correlationContent, "No correlation snapshot yet");
        return;
      }
      const rows = [];
      scopeKeys.forEach(function (scope) {
        const snap = byScope[scope];
        if (!snap) return;
        const tag = scope ? " [" + scope + "]" : "";
        rows.push('<div class="panel-row">Gross exposure' + escapeHTML(tag) + ": " +
          escapeHTML(fmtMoney(snap.portfolio_gross_usd)) + "</div>");
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
      });
      els.correlationContent.innerHTML = rows.join("");
    } catch (_err) {
      panelFallback(els.correlationContent, "-");
    }
  }

  async function refreshDeadStrategiesPanel() {
    if (!els.deadStrategiesContent) return;
    try {
      const read = await getJSONForPartition("/api/strategies/dead");
      if (!partitionStillSelected(read.partition)) return;
      const resp = read.data;
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
      const sharedNote = resp.shared
        ? '<div class="panel-row panel-muted">Shared by every partition</div>'
        : "";
      els.closingStrategiesContent.innerHTML = sharedNote + evaluators.map(function (ev) {
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

  // The table view draws no risk panel, so it reads the partition roster on its
  // own. The detail view already has it from the risk panel's status read.
  async function refreshPartitionRoster() {
    if (!els.partitionSelect) return;
    try {
      const status = await getJSON("/status");
      state.partitions = status.partitions || [];
      renderPartitionSelect();
    } catch (_err) {
      // A failed roster read leaves the last known partitions in place.
    }
  }

  function refreshOpsPanels() {
    return Promise.all([
      refreshPartitionRoster(),
      refreshLeaderboardPanel(),
      refreshDiagnosticsPanel(),
      refreshCashflowPanel(),
      refreshCorrelationPanel(),
      refreshDeadStrategiesPanel(),
      refreshClosingStrategiesPanel(),
    ]);
  }

  function handleRefreshError(err) {
    const current = tokenStampCurrent(err);
    if (err && err.status === 401) {
      if (!current) return;
      showAuthPrompt(statusToken.generation() > 0
        ? "The server rejected this token. Enter the configured status token."
        : "This server requires a status token.");
      return;
    }
    if (current && !els.authPanel.hidden && els.authMessage.textContent === AUTH_CHECKING_TEXT) {
      els.authMessage.textContent = AUTH_CHECK_FAILED_TEXT;
    }
    els.statusDot.className = "status-dot error";
    els.statusLabel.textContent = "Error";
    els.statusGrid.innerHTML = "<dt>Message</dt><dd>" + escapeHTML(err.message) + "</dd>";
  }

  async function refreshAll(options) {
    const tick = !!(options && options.tick);
    try {
      if (state.viewMode === "table") {
        await Promise.all([refreshOverview(), refreshOpsPanels()]);
        return;
      }
      await Promise.all([
        refreshChart(),
        refreshTradeHistory(),
        refreshStatus(),
        refreshRiskPanel(),
        refreshRegimeStorePanel(),
        refreshTransitionsPanel(),
        loadTunerConfig(),
        refreshFeedbackAnalysis({ tick: tick }),
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
        refreshAll({ tick: true }).catch(handleRefreshError);
      }, ms);
    }
  }

  const feedbackState = { submittingIDs: new Set(), apiSubmittingIDs: new Set(), cache: null };

  function feedbackNode(tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text !== undefined && text !== null) el.textContent = String(text);
    return el;
  }

  function setFeedbackMessage(text, kind) {
    if (!els.feedbackMessage) return;
    els.feedbackMessage.textContent = text || "";
    els.feedbackMessage.hidden = !text;
    els.feedbackMessage.className = "controls-message" + (kind ? " " + kind : "");
  }

  function feedbackAnalysesURL(id) {
    return "/api/strategies/" + encodeURIComponent(id) + "/trade-feedback/analyses";
  }

  function deepFreeze(value) {
    if (value && typeof value === "object" && !Object.isFrozen(value)) {
      Object.freeze(value);
      Object.keys(value).forEach(function (key) { deepFreeze(value[key]); });
    }
    return value;
  }

  function feedbackCacheKey(id, partition) {
    return JSON.stringify([id, partition]);
  }

  function feedbackHistorySignature(data) {
    return stableJSON({
      available: !!data.available,
      reason: data.reason || "",
      providers: data.providers || [],
      analyses: (data.analyses || []).map(function (a) {
        return [a.request_id, a.status, a.progress || ""];
      }),
    });
  }

  function feedbackReplyCurrent(ticket, partition) {
    return ticketSelected(ticket) && ticket.seq > state.lanes.feedback.accepted &&
      partitionStillSelected(partition);
  }

  function feedbackHistoryMatchesCache(id, hist) {
    const cached = feedbackState.cache;
    return !!cached && feedbackCacheKey(id, hist.partition) === cached.key &&
      !!hist.data.input_signature && hist.data.input_signature === cached.inputSignature &&
      feedbackHistorySignature(hist.data) === cached.historySignature;
  }

  function resetFeedbackPanel() {
    feedbackState.cache = null;
    if (!els.feedbackPanel) return;
    els.feedbackPanel.hidden = true;
    els.feedbackStatus.textContent = "";
    els.feedbackSummary.replaceChildren();
    els.feedbackList.replaceChildren();
    setFeedbackMessage("", "");
  }

  async function refreshFeedbackAnalysis(options) {
    if (!els.feedbackPanel) return;
    if (!state.activeID) {
      els.feedbackPanel.hidden = true;
      return;
    }
    els.feedbackPanel.hidden = false;
    const ticket = takeTicket("feedback");
    const id = ticket.id;
    const requested = state.partitionFilter;
    try {
      const cached = feedbackState.cache;
      if (options && options.tick && cached && cached.key === feedbackCacheKey(id, requested)) {
        const hist = await getJSONForPartition(feedbackAnalysesURL(id) + "?view=history");
        if (!feedbackReplyCurrent(ticket, hist.partition)) return;
        if (feedbackHistoryMatchesCache(id, hist)) {
          acceptReply("feedback", ticket);
          return;
        }
      }
      const fetched = await getJSONForPartition(feedbackAnalysesURL(id));
      if (!partitionStillSelected(fetched.partition) || !acceptReply("feedback", ticket)) return;
      feedbackState.cache = {
        key: feedbackCacheKey(id, fetched.partition),
        inputSignature: fetched.data.input_signature || "",
        historySignature: feedbackHistorySignature(fetched.data),
      };
      renderFeedbackAnalysis(id, fetched.data);
    } catch (err) {
      const partition = err && typeof err === "object" && typeof err.partition === "string"
        ? err.partition
        : requested;
      if (!partitionStillSelected(partition) || !acceptReply("feedback", ticket)) return;
      feedbackState.cache = null;
      els.feedbackStatus.textContent = "Unavailable";
      els.feedbackSummary.replaceChildren(feedbackNode("p", "tuner-note", apiFeedbackError(err)));
      els.feedbackList.replaceChildren();
    }
  }

  function apiFeedbackError(err) {
    const text = (err && err.message) || "request failed";
    try {
      const parsed = JSON.parse(text);
      if (parsed && parsed.error) return parsed.error;
    } catch (e) {
      return text;
    }
    return text;
  }

  function renderFeedbackAnalysis(id, data) {
    els.feedbackStatus.textContent = data.available ? "Ready" : "Unavailable";
    const sel = data.selection || {};
    const summary = [];
    summary.push(feedbackNode("p", "", "Included marks: " + (sel.included || 0) + " of " + (sel.candidates || 0) + " candidates (limit " + (sel.max_positions || 0) + ")"));
    if (sel.error) summary.push(feedbackNode("p", "tuner-note", "Selection: " + sel.error));
    (sel.exclusions || []).forEach(function (x) {
      summary.push(feedbackNode("p", "tuner-note", "Excluded mark " + x.feedback_rowid + " (" + x.position_id + "): " + x.reason));
    });
    if (!data.available && data.reason) summary.push(feedbackNode("p", "tuner-note", data.reason));
    (data.providers || []).forEach(function (p) {
      summary.push(feedbackNode("p", "tuner-note", "Provider " + p.id + ": " + (p.available ? "available" + (p.reason ? " (" + p.reason + ")" : "") : (p.reason || "unavailable"))));
      if (p.id === "claude_code" && p.available && data.available) summary.push(renderFeedbackLocalCommand(id));
    });
    const apiAction = renderFeedbackAPIAction(id, data);
    if (apiAction) summary.push(apiAction);
    const research = data.research || {};
    if (!research.supported) {
      summary.push(feedbackNode("p", "tuner-note", "Research: " + (research.status || "unknown") + (research.reason ? " (" + research.reason + ")" : "")));
    }
    els.feedbackSummary.replaceChildren.apply(els.feedbackSummary, summary);
    const items = (data.analyses || []).map(function (a) { return renderFeedbackAnalysisItem(id, a); });
    if (!items.length) items.push(feedbackNode("p", "tuner-note", "No analyses yet."));
    els.feedbackList.replaceChildren.apply(els.feedbackList, items);
  }

  function renderFeedbackAPIAction(id, data) {
    const provider = (data.providers || []).find(function (p) { return p.id === "anthropic_api"; });
    if (!provider) return null;
    const sel = data.selection || {};
    const box = feedbackNode("div", "feedback-analysis-proposal");
    const button = feedbackNode("button", "tuner-button secondary", "Analyze with the Anthropic API…");
    button.type = "button";
    let blocked = "";
    if (!data.available) blocked = data.reason || "analysis is unavailable";
    else if (!provider.available) blocked = provider.reason || "the Anthropic API provider is unavailable";
    else if (!(sel.included > 0)) blocked = "there are no usable saved marks to analyze";
    button.disabled = !!blocked || feedbackState.submittingIDs.has(id) || feedbackState.apiSubmittingIDs.has(id);
    button.addEventListener("click", function () { submitFeedbackAPIAnalysis(id, sel.included || 0, provider.reason || ""); });
    box.appendChild(button);
    if (blocked) box.appendChild(feedbackNode("p", "tuner-note", "Anthropic API: " + blocked));
    return box;
  }

  function settleFeedbackSubmission(ticket, strategyID) {
    if (ticketSelected(ticket)) {
      refreshFeedbackAnalysis().catch(function () {});
    } else if (state.activeID === strategyID) {
      feedbackState.cache = null;
      refreshFeedbackAnalysis().catch(function () {});
    }
  }

  async function submitFeedbackAPIAnalysis(strategyID, included, settings) {
    if (feedbackState.apiSubmittingIDs.has(strategyID) || state.activeID !== strategyID) return;
    const ticket = selectionTicket();
    const question = "Send " + included + " saved mark(s) of " + strategyID + " to the Anthropic API" +
      (settings ? " (" + settings + ")" : "") + "? This is one paid call. It is never retried by itself, and the result changes no setting.";
    if (!window.confirm(question)) {
      if (ticketSelected(ticket)) setFeedbackMessage("Cancelled. No analysis request was sent.", "");
      return;
    }
    feedbackState.apiSubmittingIDs.add(strategyID);
    let resp = null;
    let failure = null;
    try {
      resp = await postJSON(feedbackAnalysesURL(strategyID), { provider: "anthropic_api" });
    } catch (err) {
      failure = err;
    }
    feedbackState.apiSubmittingIDs.delete(strategyID);
    const requestID = (resp && resp.request_id) || "";
    if (!ticketSelected(ticket)) {
      reportLateResult("Trade mark analysis request (Anthropic API)", strategyID, failure
        ? writeOutcomeFromError(failure)
        : { status: "success", message: "Analysis " + requestID + " is accepted. Its progress shows in that strategy's analysis list." });
    } else if (failure) {
      const outcome = writeOutcomeFromError(failure);
      setFeedbackMessage(outcome.status === "unknown"
        ? describeWriteOutcome("Trade mark analysis request (Anthropic API)", strategyID, outcome)
        : "The analysis was not started: " + apiFeedbackError(failure), "error");
    } else {
      setFeedbackMessage("Analysis " + requestID + " is accepted. Its progress shows below.", "success");
    }
    settleFeedbackSubmission(ticket, strategyID);
  }

  function feedbackShellQuote(text) {
    return "'" + String(text).replace(/'/g, "'\\''") + "'";
  }

  function renderFeedbackLocalCommand(id) {
    const box = feedbackNode("div", "feedback-analysis-local");
    box.appendChild(feedbackNode("p", "", "Local Claude Code analysis: run this in your go-trader checkout. Set GO_TRADER_STATUS_TOKEN in that shell yourself; the dashboard never shows it."));
    const command = "GO_TRADER_DASHBOARD_URL=" + feedbackShellQuote(window.location.origin) +
      " claude " + feedbackShellQuote("/go-trader-trade-feedback " + id);
    const pre = feedbackNode("pre", "feedback-research-grid", command);
    pre.dataset.feedbackLocalCommand = "true";
    box.appendChild(pre);
    const button = feedbackNode("button", "tuner-button secondary", "Copy command");
    button.type = "button";
    button.addEventListener("click", function () {
      if (!navigator.clipboard || !navigator.clipboard.writeText) {
        setFeedbackMessage("Copy is not available here. Select the command and copy it.", "error");
        return;
      }
      navigator.clipboard.writeText(command).then(function () {
        setFeedbackMessage("Command copied. It holds no token.", "success");
      }, function () {
        setFeedbackMessage("Copy failed. Select the command and copy it.", "error");
      });
    });
    box.appendChild(button);
    return box;
  }

  function renderFeedbackAnalysisItem(id, a) {
    const item = feedbackNode("div", "feedback-analysis-item");
    item.dataset.requestId = a.request_id;
    const status = a.status + (a.progress ? " (" + a.progress + ")" : "");
    item.appendChild(feedbackNode("h4", "", a.request_id + ": " + status));
    item.appendChild(feedbackNode("p", "tuner-note", "Provider " + a.provider + ", model " + (a.model || "-") +
      (a.model_verification ? " [" + a.model_verification + "]" : "") + ", marks " + (a.feedback_rowids || []).join(", ")));
    if (a.error) item.appendChild(feedbackNode("p", "controls-message error", a.error));
    if (a.result_text) item.appendChild(feedbackNode("pre", "feedback-research-grid", a.result_text));
    const result = a.result && typeof a.result === "object" ? a.result : null;
    if (result) {
      if (typeof result.summary === "string") item.appendChild(feedbackNode("p", "", result.summary));
      if (typeof result.confidence === "string") item.appendChild(feedbackNode("p", "tuner-note", "Confidence: " + result.confidence));
      if (Array.isArray(result.findings) && result.findings.length) {
        const ul = feedbackNode("ul");
        result.findings.forEach(function (f) {
          ul.appendChild(feedbackNode("li", "", "Mark " + (f && f.feedback_rowid) + ": " + (f && f.text)));
        });
        item.appendChild(ul);
      }
    }
    const validation = a.validation && typeof a.validation === "object" ? a.validation : {};
    (validation.reasons || []).forEach(function (r) {
      item.appendChild(feedbackNode("p", "tuner-note", "Validation: " + r));
    });
    const proposals = result && Array.isArray(result.proposals) ? result.proposals : [];
    const research = {};
    (a.research || []).forEach(function (r) { research[r.index] = r; });
    proposals.forEach(function (p, index) {
      const box = feedbackNode("div", "feedback-analysis-proposal");
      if (p && p.kind === "param_grid") {
        box.appendChild(feedbackNode("p", "", "Proposal " + index + ": parameter grid"));
        box.appendChild(feedbackNode("pre", "feedback-research-grid", JSON.stringify(p.params, null, 2)));
        if (typeof p.rationale === "string") box.appendChild(feedbackNode("p", "tuner-note", p.rationale));
        const r = research[index] || { available: false, reason: "no research decision" };
        const button = feedbackNode("button", "tuner-button secondary", "Start research…");
        button.type = "button";
        button.dataset.proposalIndex = String(index);
        button.disabled = !r.available || feedbackState.submittingIDs.has(id) || feedbackState.apiSubmittingIDs.has(id);
        button.addEventListener("click", function () { confirmFeedbackResearch(id, a, index); });
        box.appendChild(button);
        if (!r.available && r.reason) box.appendChild(feedbackNode("p", "tuner-note", r.reason));
      } else {
        box.appendChild(feedbackNode("p", "", "Proposal " + index + ": note"));
        box.appendChild(feedbackNode("p", "tuner-note", p && p.text));
      }
      item.appendChild(box);
    });
    return item;
  }

  function showFeedbackResearchDialog() {
    return new Promise(function (resolve) {
      const dialog = els.feedbackResearchDialog;
      const onClose = function () {
        dialog.removeEventListener("close", onClose);
        resolve(dialog.returnValue);
      };
      dialog.returnValue = "";
      dialog.addEventListener("close", onClose);
      dialog.showModal();
    });
  }

  async function confirmFeedbackResearch(strategyID, analysis, index) {
    if (feedbackState.submittingIDs.has(strategyID)) return;
    const proposal = analysis.result && analysis.result.proposals && analysis.result.proposals[index];
    if (!proposal || proposal.kind !== "param_grid") return;
    const ticket = selectionTicket();
    const label = "Research run";
    const frozen = deepFreeze({
      strategyID: strategyID,
      requestID: analysis.request_id,
      index: index,
      bundle: analysis.bundle_sha256,
      params: JSON.parse(JSON.stringify(proposal.params)),
    });
    els.feedbackResearchStrategy.textContent = frozen.strategyID;
    els.feedbackResearchGrid.textContent = JSON.stringify(frozen.params, null, 2);
    const choice = await showFeedbackResearchDialog();
    if (choice !== "confirm") {
      if (ticketSelected(ticket)) setFeedbackMessage("Cancelled. No research request was sent.", "");
      return;
    }
    if (feedbackState.submittingIDs.has(frozen.strategyID)) {
      if (ticketSelected(ticket)) setFeedbackMessage("A research request for this strategy is already in progress. Nothing was sent.", "error");
      return;
    }
    feedbackState.submittingIDs.add(frozen.strategyID);
    if (ticketSelected(ticket)) {
      els.feedbackList.querySelectorAll("button").forEach(function (b) { b.disabled = true; });
    }
    const readTicket = takeTicket("feedback");
    let sent = false;
    try {
      const fresh = await getJSONForPartition(feedbackAnalysesURL(frozen.strategyID));
      const match = (fresh.data.analyses || []).find(function (x) { return x.request_id === frozen.requestID; });
      const decision = match && (match.research || []).find(function (r) { return r.index === frozen.index; });
      const current = match && match.result && match.result.proposals && match.result.proposals[frozen.index];
      if (state.activeID !== frozen.strategyID || !match || !decision || !decision.available ||
          match.bundle_sha256 !== frozen.bundle || !current || stableJSON(current.params) !== stableJSON(frozen.params)) {
        if (!ticketSelected(ticket)) {
          reportLateNotSent(label, frozen.strategyID, "the selection, the marks, the settings or the analysis changed");
          return;
        }
        setFeedbackMessage("The marks, the settings or the analysis changed. Nothing was sent. Review the proposal and confirm again.", "error");
        if (readTicket.id === frozen.strategyID && partitionStillSelected(fresh.partition) &&
            acceptReply("feedback", readTicket)) {
          renderFeedbackAnalysis(frozen.strategyID, fresh.data);
        }
        return;
      }
      const overrides = {};
      overrides[frozen.strategyID] = { params: frozen.params };
      const generation = statusToken.generation();
      sent = true;
      const res = await fetch("/api/tuning/runs", {
        method: "POST",
        headers: Object.assign({
          "Content-Type": "application/json",
          "X-Trade-Feedback-Analysis": frozen.requestID + ":" + frozen.index + ":" + frozen.bundle,
        }, authHeaders()),
        body: JSON.stringify({ strategy_ids: [frozen.strategyID], overrides: overrides }),
      });
      const text = await res.text();
      if (!res.ok) {
        let msg = text;
        try { msg = JSON.parse(text).error || text; } catch (e) { msg = text; }
        if (!ticketSelected(ticket)) {
          reportLateResult(label, frozen.strategyID, {
            status: "failed",
            httpStatus: res.status,
            message: msg,
            err: { status: res.status, tokenGeneration: generation },
          });
          return;
        }
        setFeedbackMessage("Research was not started: " + msg, "error");
        return;
      }
      let run = {};
      try { run = JSON.parse(text); } catch (e) { run = {}; }
      if (!ticketSelected(ticket)) {
        reportLateResult(label, frozen.strategyID, {
          status: "success",
          message: "Research run " + (run.id || "") + " is queued. Open /tuning to follow it.",
        });
        return;
      }
      setFeedbackMessage("Research run " + (run.id || "") + " is queued. Open /tuning to follow it.", "success");
    } catch (err) {
      if (!ticketSelected(ticket)) {
        reportLateFailure(label, frozen.strategyID, err, sent);
        return;
      }
      const outcome = writeOutcomeFromError(err);
      setFeedbackMessage(sent && outcome.status === "unknown"
        ? describeWriteOutcome(label, frozen.strategyID, outcome)
        : "Research was not started: " + apiFeedbackError(err), "error");
    } finally {
      feedbackState.submittingIDs.delete(frozen.strategyID);
      settleFeedbackSubmission(ticket, frozen.strategyID);
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

  function initDashboardView() {
    state.viewMode = loadViewMode();
    applyViewMode();
    updateDarkModeToggle();
  }

  async function loadDashboard() {
    const generation = statusToken.generation();
    const read = await getJSONForPartition("/api/strategies");
    if (generation !== statusToken.generation()) return;
    hideAuthPrompt();
    state.strategies = read.data.strategies || [];
    renderStrategies();
    const keepActive = !!state.activeID && state.strategies.some(function (s) {
      return s.id === state.activeID;
    });
    if (keepActive) {
      await refreshAll();
    } else if (state.strategies.length) {
      await selectStrategy(state.strategies[0].id);
    } else {
      if (state.activeID) setActiveStrategy("");
      await refreshAll();
    }
    scheduleRefresh();
  }

  initSidebar();
  initTradeHistoryActivation();
  updateControlsPanel(null);
  if (els.tunerForm) {
    els.tunerForm.addEventListener("input", onTunerFieldChange);
    els.tunerForm.addEventListener("change", onTunerFieldChange);
    if (els.chartTimeframe) els.chartTimeframe.addEventListener("change", onChartTimeframeChange);
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
  if (els.tradePaperCloseAll) {
    els.tradePaperCloseAll.addEventListener("click", function () {
      confirmTradeAction("paper-close", {}).catch(handleRefreshError);
    });
  }
  if (els.positions) {
    els.positions.addEventListener("click", function (event) {
      const button = event.target.closest("[data-trade-action]");
      if (!button) return;
      const action = button.dataset.tradeAction;
      let run;
      if (action === "paper-close") {
        run = confirmTradeAction("paper-close", { symbol: button.dataset.symbol || "" });
      } else if (action === "close" || action === "force-close") {
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
  if (els.addQuoteButton) {
    els.addQuoteButton.addEventListener("click", function () {
      structuralAddQuoteOwner().catch(handleRefreshError);
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
    els.tunerApply.addEventListener("click", requestTunerApply);
  }
  if (els.tunerConfirmDialog) {
    els.tunerConfirmDialog.addEventListener("close", function () {
      if (els.tunerConfirmDialog.returnValue === "apply") {
        submitConfirmedApply().catch(handleRefreshError);
      } else {
        state.tuner.pendingApply = null;
      }
    });
  }
  if (els.dashboardMessageDismiss) {
    els.dashboardMessageDismiss.addEventListener("click", function () {
      state.lateResults = [];
      renderLateResults();
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
  document.querySelectorAll(".mode-filter-button").forEach(function (button) {
    button.addEventListener("click", function () {
      state.modeFilter = button.dataset.mode;
      renderOverviewTable();
    });
  });
  if (els.partitionSelect) {
    els.partitionSelect.addEventListener("change", function () {
      setPartitionFilter(els.partitionSelect.value);
    });
  }
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
    statusToken.submit(els.authToken.value);
    els.authMessage.textContent = AUTH_CHECKING_TEXT;
    loadDashboard().catch(handleRefreshError);
  });
  initDashboardView();
  loadDashboard().catch(handleRefreshError);

  function showAuthPrompt(message) {
    els.statusDot.className = "status-dot error";
    els.statusLabel.textContent = "Token required";
    els.statusGrid.innerHTML = "<dt>API</dt><dd>Unauthorized</dd>";
    els.authMessage.textContent = message;
    if (els.authPanel.hidden) {
      els.authToken.value = statusToken.current();
      els.authPanel.hidden = false;
      els.authToken.focus();
    }
  }

  function hideAuthPrompt() {
    els.authPanel.hidden = true;
    els.authMessage.textContent = "";
  }
})();
