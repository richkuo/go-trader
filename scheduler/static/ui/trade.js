(function () {
  const params = new URLSearchParams(window.location.search);
  const strategyID = params.get("strategy") || "";
  const positionID = params.get("position") || "";

  const els = {
    title: document.getElementById("review-title"),
    subtitle: document.getElementById("review-subtitle"),
    error: document.getElementById("review-error"),
    refresh: document.getElementById("review-refresh"),
    coverage: document.getElementById("review-coverage"),
    chart: document.getElementById("review-chart"),
    legend: document.getElementById("review-legend"),
    actual: document.getElementById("review-actual"),
    eligibility: document.getElementById("review-eligibility"),
    form: document.getElementById("review-form"),
    modeEntry: document.getElementById("mode-entry"),
    modeExit: document.getElementById("mode-exit"),
    entryTime: document.getElementById("entry-time"),
    entryPrice: document.getElementById("entry-price"),
    entryRange: document.getElementById("entry-range"),
    exitTime: document.getElementById("exit-time"),
    exitPrice: document.getElementById("exit-price"),
    exitRange: document.getElementById("exit-range"),
    note: document.getElementById("review-note"),
    formMessage: document.getElementById("review-form-message"),
    save: document.getElementById("review-save"),
    annotations: document.getElementById("review-annotations"),
    fills: document.getElementById("review-fills"),
  };

  const state = {
    review: null,
    chart: null,
    series: null,
    priceLines: [],
    previewLines: [],
    mode: "",
    candlesByTime: new Map(),
    saving: false,
  };

  function authHeaders() {
    let token = "";
    try {
      token = window.localStorage.getItem("goTraderStatusToken") || "";
    } catch (_err) {
      token = "";
    }
    return token ? { Authorization: "Bearer " + token } : {};
  }

  function isDark() {
    return document.documentElement.classList.contains("dark");
  }

  function apiBase() {
    return "/api/strategies/" + encodeURIComponent(strategyID);
  }

  function fmtNum(v, digits) {
    if (v === undefined || v === null || !isFinite(v)) return "-";
    return Number(v).toLocaleString(undefined, { maximumFractionDigits: digits === undefined ? 8 : digits });
  }

  function fmtTime(unix) {
    if (!unix) return "-";
    return new Date(unix * 1000).toLocaleString(undefined, {
      year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short",
    });
  }

  function setText(el, text) {
    el.textContent = text === undefined || text === null ? "" : String(text);
  }

  function showError(text) {
    if (!text) {
      els.error.hidden = true;
      setText(els.error, "");
      return;
    }
    setText(els.error, text);
    els.error.hidden = false;
  }

  async function readJSONResponse(res) {
    const text = await res.text();
    try {
      return { body: JSON.parse(text), text: text };
    } catch (_err) {
      return { body: null, text: text };
    }
  }

  async function loadReview() {
    showError("");
    if (!strategyID || !positionID) {
      showError("The page needs strategy and position values in its address.");
      return;
    }
    const url = apiBase() + "/trade-review?position_id=" + encodeURIComponent(positionID);
    const res = await fetch(url, { headers: authHeaders(), cache: "no-store" });
    const parsed = await readJSONResponse(res);
    if (res.status === 401) {
      showError("Unauthorized. Save the status token on the dashboard, then reload this window.");
      return;
    }
    if (!parsed.body || (!res.ok && !parsed.body.fills)) {
      showError((parsed.body && parsed.body.error) || parsed.text || res.statusText);
      return;
    }
    if (!res.ok) {
      showError("Candle fetch failed: " + (parsed.body.candle_error || res.statusText));
    }
    state.review = parsed.body;
    render();
  }

  function chartOptions() {
    const dark = isDark();
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
      crosshair: { mode: LightweightCharts.CrosshairMode.Normal },
    };
  }

  function initChart() {
    if (state.chart) return;
    state.chart = LightweightCharts.createChart(els.chart, chartOptions());
    state.series = state.chart.addCandlestickSeries({
      upColor: "#0f8a5f", downColor: "#c23b3b", borderUpColor: "#0f8a5f", borderDownColor: "#c23b3b",
      wickUpColor: "#0f8a5f", wickDownColor: "#c23b3b",
    });
    state.chart.subscribeClick(onChartClick);
    new ResizeObserver(function () {
      const rect = els.chart.getBoundingClientRect();
      state.chart.resize(Math.max(300, rect.width), Math.max(320, rect.height));
    }).observe(els.chart);
  }

  function barSeconds() {
    return (state.review && state.review.window && state.review.window.bar_seconds) || 0;
  }

  function snapToBar(t) {
    const bar = barSeconds();
    if (!bar) return t;
    return Math.floor(t / bar) * bar;
  }

  function clearLines(list) {
    while (list.length) {
      state.series.removePriceLine(list.pop());
    }
  }

  function addLine(list, price, color, title, style) {
    if (!(price > 0)) return;
    list.push(state.series.createPriceLine({
      price: price, color: color, lineWidth: 2, lineStyle: style, axisLabelVisible: true, title: title,
    }));
  }

  function fillMarkers() {
    const r = state.review;
    return (r.fills || []).filter(function (f) {
      return f.role === "open" || f.role === "close";
    }).map(function (f) {
      const open = f.role === "open";
      const buy = String(f.side).toLowerCase() === "buy";
      return {
        time: snapToBar(f.time),
        position: buy ? "belowBar" : "aboveBar",
        color: open ? "#2563eb" : "#a45f08",
        shape: buy ? "arrowUp" : "arrowDown",
        text: (open ? "OPEN " : "CLOSE ") + fmtNum(f.quantity, 6) + " @ " + fmtNum(f.price),
      };
    });
  }

  function previewMarkers() {
    const out = [];
    const entry = Number(els.entryTime.value);
    const exit = Number(els.exitTime.value);
    if (entry) out.push({ time: entry, position: "belowBar", color: "#0f8a5f", shape: "circle", text: "PREFERRED ENTRY" });
    if (exit) out.push({ time: exit, position: "aboveBar", color: "#c23b3b", shape: "circle", text: "PREFERRED EXIT" });
    return out;
  }

  function drawMarkers() {
    const all = fillMarkers().concat(previewMarkers());
    all.sort(function (a, b) { return a.time - b.time; });
    state.series.setMarkers(all);
  }

  function drawPreviewLines() {
    clearLines(state.previewLines);
    addLine(state.previewLines, Number(els.entryPrice.value), "#0f8a5f", "Preferred entry", LightweightCharts.LineStyle.Dotted);
    addLine(state.previewLines, Number(els.exitPrice.value), "#c23b3b", "Preferred exit", LightweightCharts.LineStyle.Dotted);
    drawMarkers();
  }

  function renderChart() {
    initChart();
    const r = state.review;
    const candles = r.candles || [];
    state.candlesByTime = new Map(candles.map(function (c) { return [c.time, c]; }));
    state.series.setData(candles);
    clearLines(state.priceLines);
    addLine(state.priceLines, r.actual.avg_entry_price, "#2563eb", "Actual avg entry", LightweightCharts.LineStyle.Solid);
    addLine(state.priceLines, r.actual.avg_exit_price, "#a45f08", "Actual avg exit", LightweightCharts.LineStyle.Solid);
    const legend = ["Solid lines: quantity-weighted average entry and exit over every fill of the position."];
    if (r.metrics) {
      addLine(state.priceLines, r.metrics.mfe_price, "#0f8a5f", "MFE (diagnostics)", LightweightCharts.LineStyle.Dashed);
      addLine(state.priceLines, r.metrics.mae_price, "#c23b3b", "MAE (diagnostics)", LightweightCharts.LineStyle.Dashed);
      legend.push("Dashed lines: maximum favorable and adverse excursion from the close diagnostics record, separate from the averages.");
    }
    setText(els.legend, legend.join(" "));
    drawPreviewLines();
    if (candles.length) state.chart.timeScale().fitContent();
  }

  function coverageText(r) {
    const cov = r.coverage || {};
    const parts = ["Coverage: " + (cov.status || "none")];
    if (cov.expected) parts.push(cov.present + " of " + cov.expected + " bars");
    if (r.candle_source) parts.push("source " + r.candle_source);
    if (r.window) {
      parts.push(r.timeframe + " bars from " + fmtTime(r.window.effective_from) + " to " + fmtTime(r.window.effective_to));
      if (r.window.future_padding_shortened) parts.push("future padding shortened to the latest completed bar");
    }
    if (cov.reason) parts.push(cov.reason);
    return parts.join(" · ");
  }

  function renderActual() {
    const r = state.review;
    const a = r.actual || {};
    const rows = [
      ["Position", r.position_id],
      ["Symbol / side", (r.symbol || "-") + " / " + (r.side || "-")],
      ["State", r.closed ? "closed" : "open or partly closed"],
      ["Opened", fmtTime(a.opened_at)],
      ["Closed", a.closed_at ? fmtTime(a.closed_at) : "-"],
      ["Avg entry", fmtNum(a.avg_entry_price)],
      ["Avg exit", a.avg_exit_price ? fmtNum(a.avg_exit_price) : "-"],
      ["Net close result", fmtNum(a.realized_pnl, 4) + " (sum of net close results; excludes opening fees and funding)"],
      ["Opening fees", fmtNum(a.opening_fees, 4)],
      ["Funding", fmtNum(a.funding, 4)],
      ["Opened / closed qty", fmtNum(a.opened_quantity) + " / " + fmtNum(a.closed_quantity)],
      ["Close reason", a.close_reason || "-"],
    ];
    els.actual.replaceChildren();
    rows.forEach(function (row) {
      const dt = document.createElement("dt");
      setText(dt, row[0]);
      const dd = document.createElement("dd");
      setText(dd, row[1]);
      els.actual.append(dt, dd);
    });
  }

  function renderFills() {
    els.fills.replaceChildren();
    (state.review.fills || []).forEach(function (f) {
      const tr = document.createElement("tr");
      [fmtTime(f.time), f.role, f.side, fmtNum(f.price), fmtNum(f.quantity), fmtNum(f.exchange_fee, 6),
        f.role === "close" || f.role === "funding" ? fmtNum(f.net_pnl, 4) : "-", f.details || "-"].forEach(function (v) {
        const td = document.createElement("td");
        setText(td, v);
        tr.append(td);
      });
      els.fills.append(tr);
    });
  }

  function annotationBlock(rec, current) {
    const box = document.createElement("article");
    box.className = "trade-review-annotation" + (current ? " current" : "");
    const head = document.createElement("h3");
    setText(head, (current ? "Current · " : "Earlier · ") + "#" + rec.row_id + " saved " + fmtTime(Date.parse(rec.created_at) / 1000));
    const body = document.createElement("p");
    setText(body, "Entry " + fmtNum(rec.preferred_entry_price) + " at " + fmtTime(rec.preferred_entry_time) +
      " · Exit " + fmtNum(rec.preferred_exit_price) + " at " + fmtTime(rec.preferred_exit_time) +
      " · " + rec.timeframe + " bars · actual avg " + fmtNum(rec.actual_avg_entry_price) + " → " + fmtNum(rec.actual_avg_exit_price));
    box.append(head, body);
    if (rec.note) {
      const note = document.createElement("p");
      note.className = "trade-review-note";
      setText(note, rec.note);
      box.append(note);
    }
    return box;
  }

  function renderAnnotations() {
    const list = state.review.annotations || [];
    els.annotations.replaceChildren();
    if (!list.length) {
      const p = document.createElement("p");
      p.className = "panel-muted";
      setText(p, "None saved yet.");
      els.annotations.append(p);
      return;
    }
    list.forEach(function (rec, i) {
      els.annotations.append(annotationBlock(rec, i === 0));
    });
  }

  function fillTimeSelect(select) {
    const keep = select.value;
    select.replaceChildren();
    const blank = document.createElement("option");
    blank.value = "";
    setText(blank, "Choose a bar");
    select.append(blank);
    (state.review.candles || []).forEach(function (c) {
      const opt = document.createElement("option");
      opt.value = String(c.time);
      setText(opt, fmtTime(c.time) + "  (L " + fmtNum(c.low) + " · H " + fmtNum(c.high) + ")");
      select.append(opt);
    });
    if (keep && state.candlesByTime.has(Number(keep))) select.value = keep;
  }

  function defaultPrice(kind, bar) {
    const long = state.review.side === "long";
    const useLow = kind === "entry" ? long : !long;
    return useLow ? bar.low : bar.high;
  }

  function rangeText(kind) {
    const select = kind === "entry" ? els.entryTime : els.exitTime;
    const bar = state.candlesByTime.get(Number(select.value));
    return bar ? "Allowed range in this bar: " + fmtNum(bar.low) + " to " + fmtNum(bar.high) : "";
  }

  function selectBar(kind, time) {
    const bar = state.candlesByTime.get(time);
    if (!bar) return;
    const select = kind === "entry" ? els.entryTime : els.exitTime;
    const price = kind === "entry" ? els.entryPrice : els.exitPrice;
    select.value = String(time);
    price.value = String(defaultPrice(kind, bar));
    onFormChange();
  }

  function validateForm() {
    const msgs = [];
    const entryT = Number(els.entryTime.value);
    const exitT = Number(els.exitTime.value);
    [["entry", entryT, Number(els.entryPrice.value)], ["exit", exitT, Number(els.exitPrice.value)]].forEach(function (p) {
      const bar = state.candlesByTime.get(p[1]);
      if (!bar) {
        msgs.push("Choose an " + p[0] + " bar.");
      } else if (!(p[2] > 0) || p[2] < bar.low || p[2] > bar.high) {
        msgs.push("The " + p[0] + " price must be inside its bar (" + fmtNum(bar.low) + " to " + fmtNum(bar.high) + ").");
      }
    });
    if (entryT && exitT && entryT >= exitT) msgs.push("The entry bar must be earlier than the exit bar.");
    const noteLen = Array.from(els.note.value).length;
    if (noteLen > 2000) msgs.push("The note has " + noteLen + " characters; the limit is 2000.");
    return msgs;
  }

  function onFormChange() {
    setText(els.entryRange, rangeText("entry"));
    setText(els.exitRange, rangeText("exit"));
    drawPreviewLines();
    updateSaveState();
  }

  function updateSaveState() {
    const r = state.review;
    const allowed = !!(r && r.save_available);
    [els.modeEntry, els.modeExit, els.entryTime, els.entryPrice, els.exitTime, els.exitPrice, els.note].forEach(function (el) {
      el.disabled = !allowed;
    });
    if (!allowed) {
      els.save.disabled = true;
      return;
    }
    const msgs = validateForm();
    els.save.disabled = state.saving || msgs.length > 0;
    if (!state.saving) setText(els.formMessage, msgs.join(" "));
  }

  function setMode(mode) {
    state.mode = state.mode === mode ? "" : mode;
    els.modeEntry.setAttribute("aria-pressed", state.mode === "entry" ? "true" : "false");
    els.modeExit.setAttribute("aria-pressed", state.mode === "exit" ? "true" : "false");
  }

  function onChartClick(param) {
    if (!state.mode || !param || param.time === undefined || param.time === null) return;
    if (!(state.review && state.review.save_available)) return;
    selectBar(state.mode, Number(param.time));
  }

  function render() {
    const r = state.review;
    setText(els.title, "Trade review · " + r.strategy_id);
    setText(els.subtitle, (r.symbol || "-") + " " + (r.side || "") + " · position " + r.position_id + " · " + r.partition);
    document.title = "Trade review · " + r.strategy_id + " · go-trader";
    setText(els.coverage, coverageText(r));
    setText(els.eligibility, r.save_available ? "Saving is available. Each save adds a new frozen record; the newest is current." :
      "Saving is unavailable: " + (r.reason || "the server refused this position."));
    els.eligibility.classList.toggle("refused", !r.save_available);
    renderChart();
    renderActual();
    renderFills();
    renderAnnotations();
    fillTimeSelect(els.entryTime);
    fillTimeSelect(els.exitTime);
    onFormChange();
  }

  async function save(event) {
    event.preventDefault();
    if (state.saving || !(state.review && state.review.save_available)) return;
    const msgs = validateForm();
    if (msgs.length) {
      setText(els.formMessage, msgs.join(" "));
      return;
    }
    state.saving = true;
    updateSaveState();
    setText(els.formMessage, "Saving…");
    try {
      const res = await fetch(apiBase() + "/trade-feedback", {
        method: "POST",
        headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
        body: JSON.stringify({
          position_id: positionID,
          entry_time: Number(els.entryTime.value),
          entry_price: Number(els.entryPrice.value),
          exit_time: Number(els.exitTime.value),
          exit_price: Number(els.exitPrice.value),
          note: els.note.value,
        }),
      });
      const parsed = await readJSONResponse(res);
      if (!res.ok) {
        state.saving = false;
        updateSaveState();
        setText(els.formMessage, "The server refused the save (" + res.status + "): " +
          ((parsed.body && parsed.body.error) || parsed.text || res.statusText));
        return;
      }
      state.saving = false;
      els.note.value = "";
      setText(els.formMessage, "Saved annotation #" + parsed.body.annotation.row_id + ".");
      await loadReview();
      setText(els.formMessage, "Saved annotation #" + parsed.body.annotation.row_id + ".");
    } catch (err) {
      state.saving = false;
      updateSaveState();
      setText(els.formMessage, "Save failed: " + (err && err.message ? err.message : String(err)));
    }
  }

  els.modeEntry.addEventListener("click", function () { setMode("entry"); });
  els.modeExit.addEventListener("click", function () { setMode("exit"); });
  els.entryTime.addEventListener("change", function () {
    const t = Number(els.entryTime.value);
    if (t) selectBar("entry", t); else onFormChange();
  });
  els.exitTime.addEventListener("change", function () {
    const t = Number(els.exitTime.value);
    if (t) selectBar("exit", t); else onFormChange();
  });
  [els.entryPrice, els.exitPrice, els.note].forEach(function (el) {
    el.addEventListener("input", onFormChange);
  });
  els.form.addEventListener("submit", save);
  els.refresh.addEventListener("click", function () {
    loadReview().catch(function (err) { showError(String(err && err.message ? err.message : err)); });
  });

  loadReview().catch(function (err) { showError(String(err && err.message ? err.message : err)); });
})();
