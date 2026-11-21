(function () {
  "use strict";

  const UP_COLOR = "#0f8a5f";
  const DOWN_COLOR = "#c23b3b";
  const VOLUME_UP_COLOR = "rgba(15, 138, 95, 0.45)";
  const VOLUME_DOWN_COLOR = "rgba(194, 59, 59, 0.45)";
  const MAIN_PANE_STRETCH = 4;
  const VOLUME_PANE_STRETCH = 1;
  const UNAVAILABLE = "unavailable";

  function isFiniteNumber(value) {
    return typeof value === "number" && isFinite(value);
  }

  function isTradeDerivedSource(source) {
    let text = source === undefined || source === null ? "" : String(source);
    if (text.endsWith(":cached")) text = text.slice(0, -":cached".length);
    return text === "trades";
  }

  function buildBarIndex(candles, source) {
    const byTime = new Map();
    const times = [];
    const bars = [];
    (candles || []).forEach(function (bar) {
      if (!bar) return;
      const time = Number(bar.time);
      byTime.set(time, bars.length);
      times.push(time);
      bars.push(bar);
    });
    return { byTime: byTime, times: times, bars: bars, tradeDerived: isTradeDerivedSource(source) };
  }

  function readoutFor(index, time) {
    if (!index || !index.bars.length) return null;
    let position = index.bars.length - 1;
    if (time !== undefined && time !== null) {
      const found = index.byTime.get(Number(time));
      if (found === undefined) return null;
      position = found;
    }
    const bar = index.bars[position];
    const previous = position > 0 ? index.bars[position - 1] : null;
    const close = isFiniteNumber(bar.close) ? bar.close : null;
    const previousClose = previous && isFiniteNumber(previous.close) ? previous.close : null;
    let change = null;
    let changePercent = null;
    if (close !== null && previousClose !== null) {
      change = close - previousClose;
      if (previousClose !== 0) changePercent = (change / previousClose) * 100;
    }
    const hasVolume = !index.tradeDerived && Object.prototype.hasOwnProperty.call(bar, "volume") && isFiniteNumber(bar.volume);
    return {
      time: index.times[position],
      open: isFiniteNumber(bar.open) ? bar.open : null,
      high: isFiniteNumber(bar.high) ? bar.high : null,
      low: isFiniteNumber(bar.low) ? bar.low : null,
      close: close,
      change: change,
      changePercent: changePercent,
      volume: hasVolume ? bar.volume : null,
    };
  }

  function displayCandles(candles) {
    return (candles || []).map(function (bar) {
      return { time: bar.time, open: bar.open, high: bar.high, low: bar.low, close: bar.close };
    });
  }

  function displayVolume(candles, source) {
    if (isTradeDerivedSource(source)) return null;
    const list = candles || [];
    const any = list.some(function (bar) {
      return bar && isFiniteNumber(bar.volume);
    });
    if (!any) return null;
    return list.map(function (bar) {
      if (!isFiniteNumber(bar.volume)) return { time: bar.time };
      return {
        time: bar.time,
        value: bar.volume,
        color: bar.close >= bar.open ? VOLUME_UP_COLOR : VOLUME_DOWN_COLOR,
      };
    });
  }

  function sortMarkers(markers) {
    return (markers || []).map(function (marker, position) {
      return { marker: marker, position: position };
    }).sort(function (a, b) {
      const diff = Number(a.marker.time) - Number(b.marker.time);
      return diff !== 0 ? diff : a.position - b.position;
    }).map(function (entry) {
      return Object.assign({}, entry.marker);
    });
  }

  function formatValue(value) {
    if (!isFiniteNumber(value)) return UNAVAILABLE;
    return value.toLocaleString(undefined, { maximumFractionDigits: 8 });
  }

  function formatChange(change, changePercent) {
    if (!isFiniteNumber(change)) return UNAVAILABLE;
    const sign = change > 0 ? "+" : "";
    let text = sign + formatValue(change);
    if (isFiniteNumber(changePercent)) {
      text += " (" + sign + changePercent.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }) + "%)";
    } else {
      text += " (" + UNAVAILABLE + ")";
    }
    return text;
  }

  function formatTime(time) {
    if (!isFiniteNumber(time)) return "";
    return new Date(time * 1000).toLocaleString(undefined, {
      year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZone: "UTC",
    }) + " UTC";
  }

  function readoutFields(readout) {
    if (!readout) return [];
    return [
      { label: "O", value: formatValue(readout.open) },
      { label: "H", value: formatValue(readout.high) },
      { label: "L", value: formatValue(readout.low) },
      { label: "C", value: formatValue(readout.close) },
      { label: "Chg", value: formatChange(readout.change, readout.changePercent), direction: readout.change > 0 ? "up" : (readout.change < 0 ? "down" : "") },
      { label: "Vol", value: formatValue(readout.volume) },
    ];
  }

  function mergeBars(existing, incoming) {
    const byTime = new Map();
    (existing || []).forEach(function (bar) {
      if (bar && isFiniteNumber(Number(bar.time))) byTime.set(Number(bar.time), bar);
    });
    (incoming || []).forEach(function (bar) {
      if (bar && isFiniteNumber(Number(bar.time))) byTime.set(Number(bar.time), bar);
    });
    return Array.from(byTime.keys()).sort(function (a, b) {
      return a - b;
    }).map(function (time) {
      return byTime.get(time);
    });
  }

  function detectGap(lastTime, incoming, barSeconds) {
    if (!isFiniteNumber(lastTime) || !isFiniteNumber(barSeconds) || barSeconds <= 0) return false;
    const list = incoming || [];
    if (!list.length) return false;
    const first = Number(list[0].time);
    return first > lastTime + barSeconds;
  }

  function mapMarkersToBars(markers, times, barSeconds) {
    const list = times || [];
    if (!list.length || !isFiniteNumber(barSeconds) || barSeconds <= 0) return [];
    const out = [];
    (markers || []).forEach(function (marker) {
      const t = Number(marker.time);
      if (!isFiniteNumber(t)) return;
      let lo = 0;
      let hi = list.length - 1;
      let found = -1;
      while (lo <= hi) {
        const mid = (lo + hi) >> 1;
        if (list[mid] <= t) {
          found = mid;
          lo = mid + 1;
        } else {
          hi = mid - 1;
        }
      }
      if (found < 0) return;
      const open = list[found];
      if (t >= open + barSeconds) return;
      out.push(Object.assign({}, marker, { time: open, sourceTime: t }));
    });
    return out;
  }

  function backoffDelay(retryAfterSeconds, attempt) {
    const floor = isFiniteNumber(Number(retryAfterSeconds)) && Number(retryAfterSeconds) > 0 ? Number(retryAfterSeconds) * 1000 : 0;
    const n = isFiniteNumber(Number(attempt)) && Number(attempt) > 0 ? Math.min(Number(attempt), 5) : 0;
    return Math.max(floor, 2000 * Math.pow(2, n));
  }

  const logic = {
    mergeBars: mergeBars,
    detectGap: detectGap,
    mapMarkersToBars: mapMarkersToBars,
    backoffDelay: backoffDelay,
    isTradeDerivedSource: isTradeDerivedSource,
    buildBarIndex: buildBarIndex,
    readoutFor: readoutFor,
    displayCandles: displayCandles,
    displayVolume: displayVolume,
    sortMarkers: sortMarkers,
    readoutFields: readoutFields,
    formatTime: formatTime,
  };

  if (typeof module !== "undefined" && module.exports) module.exports = logic;
  if (typeof window === "undefined" || typeof document === "undefined") return;

  function themeOptions(dark) {
    return {
      layout: {
        background: { type: "solid", color: dark ? "#1a211c" : "#ffffff" },
        textColor: dark ? "#c5cec8" : "#334139",
        attributionLogo: true,
        panes: {
          separatorColor: dark ? "#3a4540" : "#d8ddd2",
          separatorHoverColor: dark ? "rgba(197, 206, 200, 0.2)" : "rgba(51, 65, 57, 0.15)",
        },
      },
      grid: {
        vertLines: { color: dark ? "#2b342f" : "#eef1eb" },
        horzLines: { color: dark ? "#2b342f" : "#eef1eb" },
      },
      rightPriceScale: { borderColor: dark ? "#3a4540" : "#d8ddd2" },
      timeScale: { borderColor: dark ? "#3a4540" : "#d8ddd2", timeVisible: true },
    };
  }

  function messageOf(err) {
    if (err && err.message) return String(err.message);
    return String(err);
  }

  function create(options) {
    const opts = options || {};
    const container = opts.container;
    const minWidth = opts.minWidth || 0;
    const minHeight = opts.minHeight || 0;
    let lib = null;
    let chart = null;
    let candleSeries = null;
    let volumeSeries = null;
    let markersApi = null;
    let observer = null;
    let mount = null;
    let readoutEl = null;
    let ok = false;
    let index = buildBarIndex([], "");
    let seriesBars = [];
    let seriesSource = "";
    let seriesClass = "";
    let formingTime = 0;
    let olderHandler = null;
    const lineGroups = new Map();

    function isDark() {
      try {
        return !!(opts.isDark && opts.isDark());
      } catch (_err) {
        return false;
      }
    }

    function report(stage, err) {
      ok = false;
      if (typeof opts.onFailure === "function") {
        try {
          opts.onFailure({ stage: stage, message: messageOf(err) });
        } catch (_err) {}
      }
      return false;
    }

    function renderReadout(time) {
      if (!readoutEl) return;
      const readout = ok ? readoutFor(index, time) : null;
      const fallback = readout || (ok ? readoutFor(index, null) : null);
      readoutEl.replaceChildren();
      if (!fallback) {
        readoutEl.hidden = true;
        return;
      }
      const head = document.createElement("span");
      head.className = "chart-readout-time";
      head.textContent = formatTime(fallback.time);
      readoutEl.append(head);
      if (formingTime && fallback.time === formingTime) {
        const tag = document.createElement("span");
        tag.className = "chart-readout-forming";
        tag.textContent = "forming";
        readoutEl.append(tag);
      }
      readoutFields(fallback).forEach(function (field) {
        const item = document.createElement("span");
        item.className = "chart-readout-item";
        const label = document.createElement("span");
        label.className = "chart-readout-label";
        label.textContent = field.label;
        const value = document.createElement("span");
        value.className = "chart-readout-value" + (field.direction ? " chart-readout-" + field.direction : "");
        value.textContent = field.value;
        item.append(label, value);
        readoutEl.append(item);
      });
      readoutEl.hidden = false;
    }

    function onCrosshairMove(param) {
      try {
        if (!param || param.time === undefined || param.time === null || !param.point) {
          renderReadout(null);
          return;
        }
        renderReadout(Number(param.time));
      } catch (err) {
        report("draw", err);
      }
    }

    function onChartClick(param) {
      if (!ok || !param || param.time === undefined || param.time === null) return;
      const time = Number(param.time);
      if (!index.byTime.has(time)) return;
      if (typeof opts.onClick === "function") {
        try {
          opts.onClick({ time: time });
        } catch (err) {
          report("draw", err);
        }
      }
    }

    function resize() {
      if (!chart || !container) return;
      const rect = container.getBoundingClientRect();
      chart.resize(Math.max(minWidth, rect.width), Math.max(minHeight, rect.height));
    }

    function onVisibleLogicalRange(range) {
      if (!range || typeof olderHandler !== "function") return;
      if (range.from < 10) {
        try {
          olderHandler(range);
        } catch (err) {
          report("draw", err);
        }
      }
    }

    function drawVolume(candles, source) {
      const volume = seriesClass === "synthetic" ? null : displayVolume(candles, source);
      if (volume) {
        if (!volumeSeries) {
          volumeSeries = chart.addSeries(lib.HistogramSeries, {
            priceFormat: { type: "volume" },
            lastValueVisible: false,
            priceLineVisible: false,
          }, 1);
          const panes = chart.panes();
          if (panes[0]) panes[0].setStretchFactor(MAIN_PANE_STRETCH);
          if (panes[1]) panes[1].setStretchFactor(VOLUME_PANE_STRETCH);
        }
        volumeSeries.setData(volume);
      } else {
        removeVolume();
      }
    }

    function drawSeries(candles) {
      candleSeries.setData(displayCandles(candles));
      drawVolume(candles, seriesSource);
      index = buildBarIndex(candles, seriesClass === "synthetic" ? "trades" : seriesSource);
    }

    function teardown() {
      if (observer) {
        try { observer.disconnect(); } catch (_err) {}
      }
      if (chart) {
        try { chart.unsubscribeClick(onChartClick); } catch (_err) {}
        try { chart.unsubscribeCrosshairMove(onCrosshairMove); } catch (_err) {}
        try { chart.timeScale().unsubscribeVisibleLogicalRangeChange(onVisibleLogicalRange); } catch (_err) {}
        try { chart.remove(); } catch (_err) {}
      }
      if (mount && mount.parentNode) mount.parentNode.removeChild(mount);
      if (readoutEl && readoutEl.parentNode) readoutEl.parentNode.removeChild(readoutEl);
      observer = null;
      chart = null;
      candleSeries = null;
      volumeSeries = null;
      markersApi = null;
      mount = null;
      readoutEl = null;
      lineGroups.clear();
      index = buildBarIndex([], "");
    }

    function removeVolume() {
      if (!volumeSeries) return;
      const series = volumeSeries;
      volumeSeries = null;
      chart.removeSeries(series);
    }

    function setup() {
      try {
        if (!container) throw new Error("The chart container is missing.");
        lib = window.LightweightCharts;
        if (!lib || typeof lib.createChart !== "function" || typeof lib.createSeriesMarkers !== "function") {
          throw new Error("The chart library did not load.");
        }
        mount = document.createElement("div");
        mount.className = "chart-mount";
        readoutEl = document.createElement("div");
        readoutEl.className = "chart-readout";
        readoutEl.hidden = true;
        readoutEl.setAttribute("aria-live", "off");
        container.append(mount, readoutEl);
        chart = lib.createChart(mount, Object.assign({}, themeOptions(isDark()), {
          crosshair: { mode: lib.CrosshairMode.Normal },
        }));
        candleSeries = chart.addSeries(lib.CandlestickSeries, {
          upColor: UP_COLOR,
          downColor: DOWN_COLOR,
          borderUpColor: UP_COLOR,
          borderDownColor: DOWN_COLOR,
          wickUpColor: UP_COLOR,
          wickDownColor: DOWN_COLOR,
        }, 0);
        markersApi = lib.createSeriesMarkers(candleSeries, []);
        chart.subscribeClick(onChartClick);
        chart.subscribeCrosshairMove(onCrosshairMove);
        chart.timeScale().subscribeVisibleLogicalRangeChange(onVisibleLogicalRange);
        observer = new ResizeObserver(function () {
          try {
            resize();
          } catch (err) {
            report("draw", err);
          }
        });
        observer.observe(container);
        resize();
        ok = true;
      } catch (err) {
        teardown();
        report("setup", err);
      }
    }

    const controller = {
      available: function () {
        return ok && chart !== null;
      },
      setData: function (data) {
        if (!chart) return false;
        try {
          const payload = data || {};
          const candles = payload.candles || [];
          candleSeries.setData(displayCandles(candles));
          const volume = displayVolume(candles, payload.source);
          if (volume) {
            if (!volumeSeries) {
              volumeSeries = chart.addSeries(lib.HistogramSeries, {
                priceFormat: { type: "volume" },
                lastValueVisible: false,
                priceLineVisible: false,
              }, 1);
              const panes = chart.panes();
              if (panes[0]) panes[0].setStretchFactor(MAIN_PANE_STRETCH);
              if (panes[1]) panes[1].setStretchFactor(VOLUME_PANE_STRETCH);
            }
            volumeSeries.setData(volume);
          } else {
            removeVolume();
          }
          index = buildBarIndex(candles, payload.source);
          ok = true;
          renderReadout(null);
          return true;
        } catch (err) {
          index = buildBarIndex([], "");
          renderReadout(null);
          return report("draw", err);
        }
      },
      setSeries: function (payload, keepRange) {
        if (!chart) return false;
        try {
          const p = payload || {};
          const previous = keepRange ? chart.timeScale().getVisibleRange() : null;
          seriesSource = p.source || "";
          seriesClass = p.sourceClass || (isTradeDerivedSource(p.source) ? "synthetic" : "market");
          formingTime = Number(p.formingTime) || 0;
          seriesBars = mergeBars([], p.candles || []);
          drawSeries(seriesBars);
          ok = true;
          renderReadout(null);
          if (!seriesBars.length) return true;
          const first = Number(seriesBars[0].time);
          const last = Number(seriesBars[seriesBars.length - 1].time);
          if (previous && Number(previous.from) <= last && Number(previous.to) >= first) {
            chart.timeScale().setVisibleRange(previous);
          } else {
            chart.timeScale().fitContent();
          }
          return true;
        } catch (err) {
          seriesBars = [];
          index = buildBarIndex([], "");
          renderReadout(null);
          return report("draw", err);
        }
      },
      prependBars: function (list) {
        if (!chart) return 0;
        try {
          const before = seriesBars.length;
          const merged = mergeBars(seriesBars, list || []);
          const added = merged.length - before;
          if (added <= 0) return 0;
          const range = chart.timeScale().getVisibleLogicalRange();
          seriesBars = merged;
          drawSeries(seriesBars);
          if (range) chart.timeScale().setVisibleLogicalRange({ from: range.from + added, to: range.to + added });
          renderReadout(null);
          return added;
        } catch (err) {
          report("draw", err);
          return 0;
        }
      },
      applyTail: function (list, nextFormingTime) {
        if (!chart) return false;
        const incoming = mergeBars([], list || []);
        formingTime = Number(nextFormingTime) || 0;
        if (!incoming.length) {
          renderReadout(null);
          return true;
        }
        const range = chart.timeScale().getVisibleLogicalRange();
        seriesBars = mergeBars(seriesBars, incoming);
        try {
          const last = index.times.length ? index.times[index.times.length - 1] : -Infinity;
          incoming.forEach(function (bar) {
            const t = Number(bar.time);
            const point = displayCandles([bar])[0];
            if (t >= last) {
              candleSeries.update(point);
            } else if (index.byTime.has(t)) {
              candleSeries.update(point, true);
            } else {
              throw new Error("a tail bar opens inside the held series");
            }
          });
          drawVolume(seriesBars, seriesSource);
          index = buildBarIndex(seriesBars, seriesClass === "synthetic" ? "trades" : seriesSource);
        } catch (_err) {
          try {
            drawSeries(seriesBars);
            if (range) chart.timeScale().setVisibleLogicalRange(range);
          } catch (err) {
            return report("draw", err);
          }
        }
        renderReadout(null);
        return true;
      },
      onNeedOlder: function (callback) {
        olderHandler = callback;
      },
      sourceClass: function () {
        return seriesClass;
      },
      barTimes: function () {
        return index.times.slice();
      },
      lastBarTime: function () {
        return index.times.length ? index.times[index.times.length - 1] : null;
      },
      firstBarTime: function () {
        return index.times.length ? index.times[0] : null;
      },
      setMarkers: function (list) {
        if (!chart) return false;
        try {
          markersApi.setMarkers(sortMarkers(list));
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      setPriceLines: function (group, lines) {
        if (!chart) return false;
        try {
          const existing = lineGroups.get(group) || [];
          lineGroups.delete(group);
          existing.forEach(function (line) {
            candleSeries.removePriceLine(line);
          });
          const styles = { solid: lib.LineStyle.Solid, dashed: lib.LineStyle.Dashed, dotted: lib.LineStyle.Dotted };
          const created = [];
          lineGroups.set(group, created);
          (lines || []).forEach(function (line) {
            created.push(candleSeries.createPriceLine({
              price: line.price,
              color: line.color,
              lineWidth: 2,
              lineStyle: styles[line.style] === undefined ? lib.LineStyle.Solid : styles[line.style],
              axisLabelVisible: true,
              title: line.title,
            }));
          });
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      applyTheme: function () {
        if (!chart) return false;
        try {
          chart.applyOptions(themeOptions(isDark()));
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      fitContent: function () {
        if (!chart) return false;
        try {
          chart.timeScale().fitContent();
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      clear: function () {
        if (!chart) return false;
        try {
          markersApi.setMarkers([]);
          Array.from(lineGroups.keys()).forEach(function (group) {
            (lineGroups.get(group) || []).forEach(function (line) {
              candleSeries.removePriceLine(line);
            });
          });
          lineGroups.clear();
          removeVolume();
          candleSeries.setData([]);
          index = buildBarIndex([], "");
          seriesBars = [];
          seriesSource = "";
          seriesClass = "";
          formingTime = 0;
          renderReadout(null);
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      destroy: function () {
        ok = false;
        teardown();
        return true;
      },
    };

    setup();
    return controller;
  }

  window.GoTraderChart = { create: create, logic: logic };
})();
