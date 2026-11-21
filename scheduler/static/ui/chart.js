(function () {
  "use strict";

  const UP_COLOR = "#0f8a5f";
  const DOWN_COLOR = "#c23b3b";
  const LINE_COLOR = "#2563eb";
  const VOLUME_UP_COLOR = "rgba(15, 138, 95, 0.45)";
  const VOLUME_DOWN_COLOR = "rgba(194, 59, 59, 0.45)";
  const HOLLOW_FILL = "rgba(0, 0, 0, 0)";
  const MAIN_PANE_STRETCH = 4;
  const VOLUME_PANE_STRETCH = 1;
  const STUDY_PANE_STRETCH = 1.5;
  const UNAVAILABLE = "unavailable";
  const PREFERENCES_VERSION = 1;
  const MAX_STUDIES = 8;
  const OVERLAY_COLORS = ["#7c3aed", "#d97706", "#0891b2", "#db2777", "#4d7c0f", "#6b7280", "#9333ea", "#b45309"];

  const CHART_TYPES = [
    { value: "candles", label: "Candles" },
    { value: "hollow_candles", label: "Hollow candles" },
    { value: "bars", label: "Bars" },
    { value: "line", label: "Line" },
    { value: "area", label: "Area" },
    { value: "baseline", label: "Baseline" },
    { value: "heikin_ashi", label: "Heikin Ashi (derived)" },
  ];
  const SCALES = [
    { value: "normal", label: "Normal" },
    { value: "log", label: "Logarithmic" },
    { value: "percent", label: "Percentage" },
  ];
  const TIME_ZONES = [
    { value: "local", label: "Local time" },
    { value: "utc", label: "UTC" },
  ];

  function periodParam(key, label, min, max, value) {
    return { key: key, label: label, min: min, max: max, step: 1, integer: true, defaultValue: value };
  }

  const STUDY_SPECS = {
    sma: { name: "SMA", title: "Simple moving average", pane: "price", params: [periodParam("period", "Period", 2, 500, 20)] },
    ema: { name: "EMA", title: "Exponential moving average", pane: "price", params: [periodParam("period", "Period", 2, 500, 20)] },
    bb: {
      name: "BB",
      title: "Bollinger Bands",
      pane: "price",
      params: [
        periodParam("period", "Period", 2, 500, 20),
        { key: "mult", label: "Width", min: 0.5, max: 5, step: 0.1, integer: false, defaultValue: 2 },
      ],
    },
    vwap: { name: "VWAP", title: "Volume-weighted average price", pane: "price", params: [] },
    rsi: { name: "RSI", title: "Relative strength index", pane: "own", params: [periodParam("period", "Period", 2, 200, 14)] },
    macd: {
      name: "MACD",
      title: "Moving average convergence divergence",
      pane: "own",
      params: [
        periodParam("fast", "Fast", 2, 200, 12),
        periodParam("slow", "Slow", 3, 500, 26),
        periodParam("signal", "Signal", 2, 200, 9),
      ],
    },
    atr: { name: "ATR", title: "Average true range", pane: "own", params: [periodParam("period", "Period", 2, 200, 14)] },
  };
  const STUDY_ORDER = ["sma", "ema", "bb", "vwap", "rsi", "macd", "atr"];

  function isFiniteNumber(value) {
    return typeof value === "number" && isFinite(value);
  }

  function hasOwn(object, key) {
    return !!object && Object.prototype.hasOwnProperty.call(object, key);
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

  function readoutPosition(index, time) {
    if (!index || !index.bars.length) return -1;
    if (time === undefined || time === null) return index.bars.length - 1;
    const found = index.byTime.get(Number(time));
    return found === undefined ? -1 : found;
  }

  function readoutFor(index, time) {
    const position = readoutPosition(index, time);
    if (position < 0) return null;
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
    const hasVolume = !index.tradeDerived && hasOwn(bar, "volume") && isFiniteNumber(bar.volume);
    return {
      time: index.times[position],
      position: position,
      open: isFiniteNumber(bar.open) ? bar.open : null,
      high: isFiniteNumber(bar.high) ? bar.high : null,
      low: isFiniteNumber(bar.low) ? bar.low : null,
      close: close,
      change: change,
      changePercent: changePercent,
      volume: hasVolume ? bar.volume : null,
    };
  }

  function validOHLC(bar) {
    return !!bar && isFiniteNumber(bar.open) && isFiniteNumber(bar.high) && isFiniteNumber(bar.low) && isFiniteNumber(bar.close);
  }

  function identity(value) {
    return value;
  }

  function displayCandles(candles, transform) {
    const fn = transform || identity;
    return (candles || []).map(function (bar) {
      if (!validOHLC(bar)) return { time: bar.time };
      return { time: bar.time, open: fn(bar.open), high: fn(bar.high), low: fn(bar.low), close: fn(bar.close) };
    });
  }

  function heikinAshi(candles) {
    const out = [];
    let prevOpen = null;
    let prevClose = null;
    (candles || []).forEach(function (bar) {
      if (!validOHLC(bar)) {
        prevOpen = null;
        prevClose = null;
        out.push(null);
        return;
      }
      const close = (bar.open + bar.high + bar.low + bar.close) / 4;
      const open = prevOpen === null ? (bar.open + bar.close) / 2 : (prevOpen + prevClose) / 2;
      const high = Math.max(bar.high, open, close);
      const low = Math.min(bar.low, open, close);
      out.push({ open: open, high: high, low: low, close: close });
      prevOpen = open;
      prevClose = close;
    });
    return out;
  }

  function displayHeikinAshi(candles, transform) {
    const fn = transform || identity;
    const derived = heikinAshi(candles);
    return (candles || []).map(function (bar, i) {
      const ha = derived[i];
      if (!ha) return { time: bar.time };
      return { time: bar.time, open: fn(ha.open), high: fn(ha.high), low: fn(ha.low), close: fn(ha.close) };
    });
  }

  function displayCloses(candles, transform) {
    const fn = transform || identity;
    return (candles || []).map(function (bar) {
      if (!bar || !isFiniteNumber(bar.close)) return { time: bar.time };
      return { time: bar.time, value: fn(bar.close) };
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

  function firstValidClose(candles) {
    const list = candles || [];
    for (let i = 0; i < list.length; i++) {
      if (list[i] && isFiniteNumber(list[i].close)) return list[i].close;
    }
    return null;
  }

  function usesOHLC(chartType) {
    return chartType === "candles" || chartType === "hollow_candles" || chartType === "bars" || chartType === "heikin_ashi";
  }

  function resolveScale(requested, candles, chartType) {
    const list = candles || [];
    if (requested === "log") {
      let any = false;
      for (let i = 0; i < list.length; i++) {
        const bar = list[i];
        if (usesOHLC(chartType)) {
          if (!validOHLC(bar)) continue;
          any = true;
          if (!(bar.low > 0)) {
            return { scale: "normal", reason: "The logarithmic scale needs every price above zero. The chart uses the normal scale until such bars load." };
          }
        } else {
          if (!bar || !isFiniteNumber(bar.close)) continue;
          any = true;
          if (!(bar.close > 0)) {
            return { scale: "normal", reason: "The logarithmic scale needs every close above zero. The chart uses the normal scale until such bars load." };
          }
        }
      }
      if (!any) return { scale: "normal", reason: list.length ? "The logarithmic scale needs at least one valid bar. The chart uses the normal scale until one loads." : "" };
      return { scale: "log", reason: "" };
    }
    if (requested === "percent") {
      const reference = firstValidClose(list);
      if (reference === null) {
        return { scale: "normal", reason: list.length ? "The percentage scale needs a valid first close. The chart uses the normal scale until one loads." : "" };
      }
      if (!(reference > 0)) {
        return { scale: "normal", reason: "The percentage scale needs a first close above zero. The chart uses the normal scale until such bars load." };
      }
      return { scale: "percent", reason: "", reference: reference };
    }
    return { scale: "normal", reason: "" };
  }

  function percentTransform(reference) {
    return function (value) {
      if (!isFiniteNumber(value)) return value;
      return (value / reference - 1) * 100;
    };
  }

  function closeValues(candles) {
    return (candles || []).map(function (bar) {
      return bar && isFiniteNumber(bar.close) ? bar.close : null;
    });
  }

  function smaValues(values, period) {
    const out = [];
    let run = 0;
    let sum = 0;
    for (let i = 0; i < values.length; i++) {
      const v = values[i];
      if (v === null) {
        run = 0;
        sum = 0;
        out.push(null);
        continue;
      }
      run += 1;
      sum += v;
      if (run > period) sum -= values[i - period];
      out.push(run >= period ? sum / period : null);
    }
    return out;
  }

  function emaValues(values, period) {
    const out = [];
    const alpha = 2 / (period + 1);
    let run = 0;
    let seed = 0;
    let prev = null;
    for (let i = 0; i < values.length; i++) {
      const v = values[i];
      if (v === null) {
        run = 0;
        seed = 0;
        prev = null;
        out.push(null);
        continue;
      }
      if (prev === null) {
        run += 1;
        seed += v;
        if (run === period) {
          prev = seed / period;
          out.push(prev);
        } else {
          out.push(null);
        }
        continue;
      }
      prev = prev + alpha * (v - prev);
      out.push(prev);
    }
    return out;
  }

  function bollingerValues(values, period, mult) {
    const middle = smaValues(values, period);
    const upper = [];
    const lower = [];
    for (let i = 0; i < values.length; i++) {
      if (middle[i] === null) {
        upper.push(null);
        lower.push(null);
        continue;
      }
      let sq = 0;
      for (let j = i - period + 1; j <= i; j++) {
        const d = values[j] - middle[i];
        sq += d * d;
      }
      const sd = Math.sqrt(sq / period);
      upper.push(middle[i] + mult * sd);
      lower.push(middle[i] - mult * sd);
    }
    return { upper: upper, middle: middle, lower: lower };
  }

  function vwapValues(candles) {
    const out = [];
    let broken = false;
    let pv = 0;
    let vol = 0;
    (candles || []).forEach(function (bar) {
      if (broken) {
        out.push(null);
        return;
      }
      const ok = !!bar && isFiniteNumber(bar.high) && isFiniteNumber(bar.low) && isFiniteNumber(bar.close) &&
        hasOwn(bar, "volume") && isFiniteNumber(bar.volume) && bar.volume >= 0;
      if (!ok) {
        broken = true;
        out.push(null);
        return;
      }
      pv += ((bar.high + bar.low + bar.close) / 3) * bar.volume;
      vol += bar.volume;
      out.push(vol > 0 ? pv / vol : null);
    });
    return out;
  }

  function rsiValues(values, period) {
    const out = [];
    let prev = null;
    let count = 0;
    let gainSum = 0;
    let lossSum = 0;
    let avgGain = null;
    let avgLoss = null;
    for (let i = 0; i < values.length; i++) {
      const v = values[i];
      if (v === null) {
        prev = null;
        count = 0;
        gainSum = 0;
        lossSum = 0;
        avgGain = null;
        avgLoss = null;
        out.push(null);
        continue;
      }
      if (prev === null) {
        prev = v;
        out.push(null);
        continue;
      }
      const change = v - prev;
      prev = v;
      const gain = change > 0 ? change : 0;
      const loss = change < 0 ? -change : 0;
      if (avgGain === null) {
        count += 1;
        gainSum += gain;
        lossSum += loss;
        if (count < period) {
          out.push(null);
          continue;
        }
        avgGain = gainSum / period;
        avgLoss = lossSum / period;
      } else {
        avgGain = (avgGain * (period - 1) + gain) / period;
        avgLoss = (avgLoss * (period - 1) + loss) / period;
      }
      if (avgLoss === 0 && avgGain === 0) {
        out.push(null);
      } else if (avgLoss === 0) {
        out.push(100);
      } else {
        out.push(100 - 100 / (1 + avgGain / avgLoss));
      }
    }
    return out;
  }

  function atrValues(candles, period) {
    const out = [];
    let prevClose = null;
    let count = 0;
    let sum = 0;
    let atr = null;
    (candles || []).forEach(function (bar) {
      if (!bar || !isFiniteNumber(bar.high) || !isFiniteNumber(bar.low) || !isFiniteNumber(bar.close)) {
        prevClose = null;
        count = 0;
        sum = 0;
        atr = null;
        out.push(null);
        return;
      }
      let tr = bar.high - bar.low;
      if (prevClose !== null) {
        tr = Math.max(tr, Math.abs(bar.high - prevClose), Math.abs(bar.low - prevClose));
      }
      prevClose = bar.close;
      if (atr === null) {
        count += 1;
        sum += tr;
        if (count < period) {
          out.push(null);
          return;
        }
        atr = sum / period;
      } else {
        atr = (atr * (period - 1) + tr) / period;
      }
      out.push(atr);
    });
    return out;
  }

  function macdValues(values, fast, slow, signal) {
    const fastLine = emaValues(values, fast);
    const slowLine = emaValues(values, slow);
    const macd = values.map(function (_v, i) {
      return fastLine[i] === null || slowLine[i] === null ? null : fastLine[i] - slowLine[i];
    });
    const signalLine = emaValues(macd, signal);
    const histogram = macd.map(function (v, i) {
      return v === null || signalLine[i] === null ? null : v - signalLine[i];
    });
    return { macd: macd, signal: signalLine, histogram: histogram };
  }

  function studySpec(type) {
    return hasOwn(STUDY_SPECS, type) ? STUDY_SPECS[type] : null;
  }

  function formatParam(value) {
    return isFiniteNumber(value) ? String(value) : "";
  }

  function validateParam(param, raw) {
    const text = raw === undefined || raw === null ? "" : String(raw).trim();
    const value = typeof raw === "number" ? raw : (text === "" ? NaN : Number(text));
    const range = param.label + " must be " + (param.integer ? "a whole number" : "a number") + " from " + param.min + " to " + param.max + ".";
    if (!isFiniteNumber(value)) return { ok: false, error: range };
    if (param.integer && Math.floor(value) !== value) return { ok: false, error: range };
    if (value < param.min || value > param.max) return { ok: false, error: range };
    return { ok: true, value: param.integer ? value : Math.round(value * 1000) / 1000 };
  }

  function validateStudyParams(type, params) {
    const spec = studySpec(type);
    if (!spec) return { ok: false, error: "Unknown study." };
    const source = params && typeof params === "object" && !Array.isArray(params) ? params : {};
    const out = {};
    for (let i = 0; i < spec.params.length; i++) {
      const param = spec.params[i];
      const check = validateParam(param, hasOwn(source, param.key) ? source[param.key] : undefined);
      if (!check.ok) return { ok: false, error: check.error };
      out[param.key] = check.value;
    }
    if (type === "macd" && !(out.fast < out.slow)) {
      return { ok: false, error: "Fast must be smaller than Slow." };
    }
    return { ok: true, params: out };
  }

  function defaultStudyParams(type) {
    const spec = studySpec(type);
    const out = {};
    (spec ? spec.params : []).forEach(function (param) {
      out[param.key] = param.defaultValue;
    });
    return out;
  }

  function studyLabel(type, params) {
    const spec = studySpec(type);
    if (!spec) return "";
    if (type === "vwap") return "VWAP (from first loaded bar)";
    const values = spec.params.map(function (param) {
      return params && hasOwn(params, param.key) ? formatParam(params[param.key]) : "?";
    });
    return spec.name + " " + values.join(" ");
  }

  function studyWarmup(type, params) {
    switch (type) {
      case "sma":
      case "ema":
      case "bb":
      case "atr":
        return params.period;
      case "rsi":
        return params.period + 1;
      case "macd":
        return params.slow + params.signal - 1;
      default:
        return 1;
    }
  }

  function computeStudy(type, params, candles) {
    const values = closeValues(candles);
    let outputs;
    switch (type) {
      case "sma":
        outputs = [{ key: "value", label: studyLabel(type, params), kind: "line", values: smaValues(values, params.period) }];
        break;
      case "ema":
        outputs = [{ key: "value", label: studyLabel(type, params), kind: "line", values: emaValues(values, params.period) }];
        break;
      case "bb": {
        const bands = bollingerValues(values, params.period, params.mult);
        outputs = [
          { key: "upper", label: "BB upper", kind: "line", values: bands.upper },
          { key: "middle", label: "BB middle", kind: "line", values: bands.middle },
          { key: "lower", label: "BB lower", kind: "line", values: bands.lower },
        ];
        break;
      }
      case "vwap":
        outputs = [{ key: "value", label: "VWAP", kind: "line", values: vwapValues(candles) }];
        break;
      case "rsi":
        outputs = [{ key: "value", label: studyLabel(type, params), kind: "line", values: rsiValues(values, params.period) }];
        break;
      case "macd": {
        const macd = macdValues(values, params.fast, params.slow, params.signal);
        outputs = [
          { key: "macd", label: "MACD", kind: "line", values: macd.macd },
          { key: "signal", label: "Signal", kind: "line", values: macd.signal },
          { key: "histogram", label: "Hist", kind: "histogram", values: macd.histogram },
        ];
        break;
      }
      case "atr":
        outputs = [{ key: "value", label: studyLabel(type, params), kind: "line", values: atrValues(candles, params.period) }];
        break;
      default:
        return { outputs: [], reason: "Unknown study." };
    }
    const any = outputs.some(function (output) {
      return output.values.some(function (v) { return v !== null; });
    });
    let reason = "";
    if (!any) {
      reason = type === "vwap"
        ? "Unavailable: the first loaded bar has no usable volume, or the cumulative volume is zero."
        : "Unavailable: needs " + studyWarmup(type, params) + " consecutive valid bars.";
    }
    return { outputs: outputs, reason: reason };
  }

  function defaultPreferences() {
    return {
      chartType: "candles",
      scale: "normal",
      timeZone: "utc",
      lastPriceLine: true,
      lastPriceLabel: true,
      studies: [],
    };
  }

  function allowed(list, value) {
    return list.some(function (entry) { return entry.value === value; });
  }

  function sanitizePreferences(value) {
    const prefs = defaultPreferences();
    if (!value || typeof value !== "object" || Array.isArray(value) || value.version !== PREFERENCES_VERSION) return prefs;
    if (allowed(CHART_TYPES, value.chart_type)) prefs.chartType = value.chart_type;
    if (allowed(SCALES, value.scale)) prefs.scale = value.scale;
    if (allowed(TIME_ZONES, value.time_zone)) prefs.timeZone = value.time_zone;
    if (typeof value.last_price_line === "boolean") prefs.lastPriceLine = value.last_price_line;
    if (typeof value.last_price_label === "boolean") prefs.lastPriceLabel = value.last_price_label;
    if (Array.isArray(value.studies)) {
      value.studies.forEach(function (entry) {
        if (prefs.studies.length >= MAX_STUDIES) return;
        if (!entry || typeof entry !== "object" || typeof entry.type !== "string" || !studySpec(entry.type)) return;
        const check = validateStudyParams(entry.type, entry.params);
        if (!check.ok) return;
        prefs.studies.push({ type: entry.type, params: check.params });
      });
    }
    return prefs;
  }

  function parsePreferences(text) {
    if (typeof text !== "string" || text === "") return defaultPreferences();
    try {
      return sanitizePreferences(JSON.parse(text));
    } catch (_err) {
      return defaultPreferences();
    }
  }

  function serializePreferences(prefs) {
    const clean = sanitizePreferences({
      version: PREFERENCES_VERSION,
      chart_type: prefs.chartType,
      scale: prefs.scale,
      time_zone: prefs.timeZone,
      last_price_line: prefs.lastPriceLine,
      last_price_label: prefs.lastPriceLabel,
      studies: (prefs.studies || []).map(function (study) {
        return { type: study.type, params: study.params };
      }),
    });
    return JSON.stringify({
      version: PREFERENCES_VERSION,
      chart_type: clean.chartType,
      scale: clean.scale,
      time_zone: clean.timeZone,
      last_price_line: clean.lastPriceLine,
      last_price_label: clean.lastPriceLabel,
      studies: clean.studies.map(function (study) {
        const params = {};
        studySpec(study.type).params.forEach(function (param) {
          params[param.key] = study.params[param.key];
        });
        return { type: study.type, params: params };
      }),
    });
  }

  function formatValue(value) {
    if (!isFiniteNumber(value)) return UNAVAILABLE;
    return value.toLocaleString(undefined, { maximumFractionDigits: 8 });
  }

  function formatStudyValue(value) {
    if (!isFiniteNumber(value)) return UNAVAILABLE;
    return value.toLocaleString(undefined, { maximumSignificantDigits: 7 });
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

  function formatTime(time, zone) {
    if (!isFiniteNumber(time)) return "";
    const local = zone === "local";
    const options = { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" };
    if (local) {
      options.timeZoneName = "short";
    } else {
      options.timeZone = "UTC";
    }
    const text = new Date(time * 1000).toLocaleString(undefined, options);
    return local ? text : text + " UTC";
  }

  function formatTick(time, tickType, zone) {
    const seconds = Number(time);
    if (!isFiniteNumber(seconds)) return "";
    const options = zone === "local" ? {} : { timeZone: "UTC" };
    switch (tickType) {
      case 0:
        options.year = "numeric";
        break;
      case 1:
        options.month = "short";
        break;
      case 2:
        options.day = "numeric";
        break;
      case 4:
        options.hour = "2-digit";
        options.minute = "2-digit";
        options.second = "2-digit";
        options.hourCycle = "h23";
        break;
      default:
        options.hour = "2-digit";
        options.minute = "2-digit";
        options.hourCycle = "h23";
    }
    return new Date(seconds * 1000).toLocaleString(undefined, options);
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

  function exportFileName(name, now) {
    const safe = String(name || "chart").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 80) || "chart";
    const stamp = new Date(now).toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
    return "go-trader-" + safe + "-" + stamp + ".png";
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
    return Number(list[0].time) > lastTime + barSeconds;
  }

  function gapRecoveryOutcome(lastTime, reply, tailFirstTime, barSeconds, limit) {
    if (!isFiniteNumber(lastTime) || !isFiniteNumber(tailFirstTime) || !isFiniteNumber(barSeconds) || barSeconds <= 0) return "reload";
    const bars = (reply || []).filter(function (c) {
      return Number(c.time) >= lastTime;
    });
    if (!bars.length) return "reload";
    const first = Number(bars[0].time);
    const newest = Number(bars[bars.length - 1].time);
    if (first > lastTime + barSeconds || newest <= lastTime) return "reload";
    if (newest + barSeconds >= tailFirstTime) return "bridged";
    if (isFiniteNumber(limit) && limit > 0 && (reply || []).length >= limit) return "partial";
    return "reload";
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
    gapRecoveryOutcome: gapRecoveryOutcome,
    mapMarkersToBars: mapMarkersToBars,
    backoffDelay: backoffDelay,
    isTradeDerivedSource: isTradeDerivedSource,
    buildBarIndex: buildBarIndex,
    readoutFor: readoutFor,
    displayCandles: displayCandles,
    displayHeikinAshi: displayHeikinAshi,
    displayCloses: displayCloses,
    displayVolume: displayVolume,
    heikinAshi: heikinAshi,
    sortMarkers: sortMarkers,
    readoutFields: readoutFields,
    formatTime: formatTime,
    formatTick: formatTick,
    resolveScale: resolveScale,
    percentTransform: percentTransform,
    smaValues: smaValues,
    emaValues: emaValues,
    bollingerValues: bollingerValues,
    vwapValues: vwapValues,
    rsiValues: rsiValues,
    atrValues: atrValues,
    macdValues: macdValues,
    computeStudy: computeStudy,
    validateStudyParams: validateStudyParams,
    studyLabel: studyLabel,
    defaultPreferences: defaultPreferences,
    sanitizePreferences: sanitizePreferences,
    parsePreferences: parsePreferences,
    serializePreferences: serializePreferences,
    exportFileName: exportFileName,
    STUDY_SPECS: STUDY_SPECS,
    MAX_STUDIES: MAX_STUDIES,
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

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function create(options) {
    const opts = options || {};
    const container = opts.container;
    const toolbar = opts.toolbar || null;
    const fullscreenTarget = opts.fullscreenTarget || container;
    const accessor = opts.preferences || null;
    const minWidth = opts.minWidth || 0;
    const minHeight = opts.minHeight || 0;
    let lib = null;
    let chart = null;
    let priceSeries = null;
    let volumeSeries = null;
    let studySeries = [];
    let markersApi = null;
    let observer = null;
    let mount = null;
    let readoutEl = null;
    let ok = false;
    let index = buildBarIndex([], "");
    let markerDefs = [];
    let effectiveScale = { scale: "normal", reason: "" };
    let transform = identity;
    let studyResults = new Map();
    let nextStudyID = 1;
    let fullscreenListener = null;
    let seriesClass = "";
    let formingTime = 0;
    let olderHandler = null;
    const lineDefs = new Map();
    const lineHandles = new Map();
    const controls = {};
    const prefs = loadPreferences();
    const studies = prefs.studies.map(function (study) {
      return newStudy(study.type, study.params);
    });

    function newStudy(type, params) {
      const id = "study-" + nextStudyID;
      nextStudyID += 1;
      return { id: id, type: type, params: Object.assign({}, params), drafts: {}, error: "", drawError: "", dom: null };
    }

    function loadPreferences() {
      if (!accessor || typeof accessor.read !== "function") return defaultPreferences();
      let read = null;
      try {
        read = accessor.read();
      } catch (_err) {
        read = null;
      }
      if (!read || !read.ok || typeof read.value !== "string") return defaultPreferences();
      return parsePreferences(read.value);
    }

    function currentPreferences() {
      return {
        chartType: prefs.chartType,
        scale: prefs.scale,
        timeZone: prefs.timeZone,
        lastPriceLine: prefs.lastPriceLine,
        lastPriceLabel: prefs.lastPriceLabel,
        studies: studies.filter(function (study) {
          return validateStudyParams(study.type, study.params).ok;
        }).map(function (study) {
          return { type: study.type, params: study.params };
        }),
      };
    }

    function savePreferences() {
      let result = null;
      try {
        if (accessor && typeof accessor.write === "function") result = accessor.write(serializePreferences(currentPreferences()));
      } catch (_err) {
        result = null;
      }
      setNote(controls.storageNote, result && result.ok ? "" :
        "Chart settings could not be saved in this browser. They stay active on this page until it reloads.");
    }

    function setNote(node, text) {
      if (!node) return;
      node.textContent = text || "";
      node.hidden = !text;
    }

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

    function studyHidden() {
      return index.tradeDerived;
    }

    function readoutStudyFields(position) {
      const fields = [];
      if (position < 0 || studyHidden()) return fields;
      studies.forEach(function (study) {
        const result = studyResults.get(study.id);
        if (!result || study.error || study.drawError) return;
        result.outputs.forEach(function (output) {
          fields.push({ label: output.label, value: formatStudyValue(output.values[position]) });
        });
      });
      return fields;
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
      let head = formatTime(fallback.time, prefs.timeZone);
      if (prefs.chartType === "heikin_ashi") head += " · raw bar";
      readoutEl.append(el("span", "chart-readout-time", head));
      if (formingTime && fallback.time === formingTime) readoutEl.append(el("span", "chart-readout-forming", "forming"));
      readoutFields(fallback).concat(readoutStudyFields(fallback.position)).forEach(function (field) {
        const item = el("span", "chart-readout-item");
        const value = el("span", "chart-readout-value" + (field.direction ? " chart-readout-" + field.direction : ""), field.value);
        item.append(el("span", "chart-readout-label", field.label), value);
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
      if (!range || typeof olderHandler !== "function" || !index.bars.length) return;
      if (range.from < 10) {
        try {
          olderHandler(range);
        } catch (err) {
          report("draw", err);
        }
      }
    }

    function seriesSourceLabel() {
      return seriesClass === "synthetic" ? "trades" : "";
    }

    function teardown() {
      if (observer) {
        try { observer.disconnect(); } catch (_err) {}
      }
      if (fullscreenListener) {
        try { document.removeEventListener("fullscreenchange", fullscreenListener); } catch (_err) {}
      }
      if (chart) {
        try { chart.unsubscribeClick(onChartClick); } catch (_err) {}
        try { chart.unsubscribeCrosshairMove(onCrosshairMove); } catch (_err) {}
        try { chart.timeScale().unsubscribeVisibleLogicalRangeChange(onVisibleLogicalRange); } catch (_err) {}
        try { chart.remove(); } catch (_err) {}
      }
      if (mount && mount.parentNode) mount.parentNode.removeChild(mount);
      if (readoutEl && readoutEl.parentNode) readoutEl.parentNode.removeChild(readoutEl);
      if (toolbar) {
        toolbar.replaceChildren();
        toolbar.hidden = true;
      }
      observer = null;
      fullscreenListener = null;
      chart = null;
      priceSeries = null;
      volumeSeries = null;
      studySeries = [];
      markersApi = null;
      mount = null;
      readoutEl = null;
      lineDefs.clear();
      lineHandles.clear();
      markerDefs = [];
      studyResults = new Map();
      index = buildBarIndex([], "");
    }

    function priceFormatOptions() {
      if (effectiveScale.scale !== "percent") return {};
      return {
        priceFormat: {
          type: "custom",
          minMove: 0.01,
          formatter: function (value) {
            return isFiniteNumber(value) ? value.toFixed(2) + "%" : "";
          },
        },
      };
    }

    function priceSeriesSpec() {
      const common = Object.assign({
        lastValueVisible: !!prefs.lastPriceLabel,
        priceLineVisible: !!prefs.lastPriceLine,
      }, priceFormatOptions());
      const candle = {
        upColor: UP_COLOR,
        downColor: DOWN_COLOR,
        borderUpColor: UP_COLOR,
        borderDownColor: DOWN_COLOR,
        wickUpColor: UP_COLOR,
        wickDownColor: DOWN_COLOR,
      };
      const bars = index.bars;
      switch (prefs.chartType) {
        case "hollow_candles":
          return { ctor: lib.CandlestickSeries, options: Object.assign({}, candle, { upColor: HOLLOW_FILL }, common), data: displayCandles(bars, transform) };
        case "bars":
          return { ctor: lib.BarSeries, options: Object.assign({ upColor: UP_COLOR, downColor: DOWN_COLOR, thinBars: false }, common), data: displayCandles(bars, transform) };
        case "line":
          return { ctor: lib.LineSeries, options: Object.assign({ color: LINE_COLOR, lineWidth: 2 }, common), data: displayCloses(bars, transform) };
        case "area":
          return {
            ctor: lib.AreaSeries,
            options: Object.assign({ lineColor: LINE_COLOR, topColor: "rgba(37, 99, 235, 0.32)", bottomColor: "rgba(37, 99, 235, 0.02)", lineWidth: 2 }, common),
            data: displayCloses(bars, transform),
          };
        case "baseline": {
          const base = firstValidClose(bars);
          return {
            ctor: lib.BaselineSeries,
            options: Object.assign({
              baseValue: { type: "price", price: base === null ? 0 : transform(base) },
              topLineColor: UP_COLOR,
              topFillColor1: "rgba(15, 138, 95, 0.28)",
              topFillColor2: "rgba(15, 138, 95, 0.04)",
              bottomLineColor: DOWN_COLOR,
              bottomFillColor1: "rgba(194, 59, 59, 0.04)",
              bottomFillColor2: "rgba(194, 59, 59, 0.28)",
              lineWidth: 2,
            }, common),
            data: displayCloses(bars, transform),
          };
        }
        case "heikin_ashi":
          return { ctor: lib.CandlestickSeries, options: Object.assign({}, candle, common), data: displayHeikinAshi(bars, transform) };
        default:
          return { ctor: lib.CandlestickSeries, options: Object.assign({}, candle, common), data: displayCandles(bars, transform) };
      }
    }

    function removeAllSeries() {
      lineHandles.clear();
      if (markersApi) {
        const api = markersApi;
        markersApi = null;
        try { api.detach(); } catch (_err) {}
      }
      const list = studySeries;
      studySeries = [];
      list.forEach(function (series) {
        chart.removeSeries(series);
      });
      if (volumeSeries) {
        const series = volumeSeries;
        volumeSeries = null;
        chart.removeSeries(series);
      }
      if (priceSeries) {
        const series = priceSeries;
        priceSeries = null;
        chart.removeSeries(series);
      }
      const panes = chart.panes();
      for (let i = panes.length - 1; i >= 1; i--) {
        chart.removePane(i);
      }
    }

    function lineStyle(style) {
      const styles = { solid: lib.LineStyle.Solid, dashed: lib.LineStyle.Dashed, dotted: lib.LineStyle.Dotted };
      return styles[style] === undefined ? lib.LineStyle.Solid : styles[style];
    }

    function attachLines(group) {
      const defs = lineDefs.get(group) || [];
      const created = [];
      lineHandles.set(group, created);
      defs.forEach(function (line) {
        const price = transform(line.price);
        if (!isFiniteNumber(price)) return;
        const title = effectiveScale.scale === "percent" ? line.title + " " + formatValue(line.price) : line.title;
        created.push(priceSeries.createPriceLine({
          price: price,
          color: line.color,
          lineWidth: 2,
          lineStyle: lineStyle(line.style),
          axisLabelVisible: true,
          title: title,
        }));
      });
    }

    function detachLines(group) {
      const existing = lineHandles.get(group) || [];
      lineHandles.delete(group);
      existing.forEach(function (line) {
        priceSeries.removePriceLine(line);
      });
    }

    function studyData(values, kind, useTransform) {
      const fn = useTransform ? transform : identity;
      return index.bars.map(function (bar, i) {
        const v = values[i];
        if (v === null || !isFiniteNumber(v)) return { time: bar.time };
        if (kind === "histogram") return { time: bar.time, value: v, color: v >= 0 ? VOLUME_UP_COLOR : VOLUME_DOWN_COLOR };
        return { time: bar.time, value: fn(v) };
      });
    }

    function overlayColor(position, offset) {
      return OVERLAY_COLORS[(position + offset) % OVERLAY_COLORS.length];
    }

    function drawStudies(firstFreePane) {
      let nextPane = firstFreePane;
      const studyPanes = [];
      studyResults = new Map();
      studies.forEach(function (study, position) {
        study.drawError = "";
        if (study.error || studyHidden()) return;
        const result = computeStudy(study.type, study.params, index.bars);
        studyResults.set(study.id, result);
        const spec = studySpec(study.type);
        const inPrice = spec.pane === "price";
        const pane = inPrice ? 0 : nextPane;
        const created = [];
        try {
          result.outputs.forEach(function (output, outputIndex) {
            const base = { lastValueVisible: false, priceLineVisible: false, crosshairMarkerVisible: false };
            let series;
            if (output.kind === "histogram") {
              series = chart.addSeries(lib.HistogramSeries, base, pane);
            } else {
              const color = study.type === "bb" && output.key === "middle"
                ? overlayColor(position, 0)
                : (study.type === "macd" ? (output.key === "macd" ? LINE_COLOR : "#d97706") : overlayColor(position, 0));
              series = chart.addSeries(lib.LineSeries, Object.assign({}, base, {
                color: color,
                lineWidth: study.type === "bb" && output.key !== "middle" ? 1 : 2,
                lineStyle: study.type === "bb" && output.key !== "middle" ? lib.LineStyle.Dashed : lib.LineStyle.Solid,
              }, inPrice ? priceFormatOptions() : {}), pane);
            }
            created.push(series);
            series.setData(studyData(output.values, output.kind, inPrice));
            if (study.type === "rsi" && outputIndex === 0) {
              [70, 30].forEach(function (level) {
                series.createPriceLine({ price: level, color: "#9ca3af", lineWidth: 1, lineStyle: lib.LineStyle.Dotted, axisLabelVisible: false, title: "" });
              });
            }
          });
          studySeries = studySeries.concat(created);
          if (!inPrice) {
            studyPanes.push(pane);
            nextPane += 1;
          }
        } catch (err) {
          study.drawError = "Unavailable: the study could not draw (" + messageOf(err) + ").";
          studyResults.delete(study.id);
          created.forEach(function (series) {
            try { chart.removeSeries(series); } catch (_err) {}
          });
          studySeries = studySeries.filter(function (series) { return created.indexOf(series) < 0; });
        }
      });
      return studyPanes;
    }

    function rebuild(keepRange) {
      let range = null;
      if (keepRange && index.bars.length) {
        try { range = chart.timeScale().getVisibleLogicalRange(); } catch (_err) { range = null; }
      }
      removeAllSeries();
      effectiveScale = resolveScale(prefs.scale, index.bars, prefs.chartType);
      transform = effectiveScale.scale === "percent" ? percentTransform(effectiveScale.reference) : identity;
      const spec = priceSeriesSpec();
      priceSeries = chart.addSeries(spec.ctor, spec.options, 0);
      priceSeries.setData(spec.data);
      priceSeries.priceScale().applyOptions({
        mode: effectiveScale.scale === "log" ? lib.PriceScaleMode.Logarithmic : lib.PriceScaleMode.Normal,
      });
      markersApi = lib.createSeriesMarkers(priceSeries, markerDefs);
      Array.from(lineDefs.keys()).forEach(attachLines);
      let nextPane = 1;
      const volume = displayVolume(index.bars, index.tradeDerived ? "trades" : "");
      if (volume) {
        volumeSeries = chart.addSeries(lib.HistogramSeries, {
          priceFormat: { type: "volume" },
          lastValueVisible: false,
          priceLineVisible: false,
        }, 1);
        volumeSeries.setData(volume);
        nextPane = 2;
      }
      const studyPanes = drawStudies(nextPane);
      const panes = chart.panes();
      if (panes[0]) panes[0].setStretchFactor(MAIN_PANE_STRETCH);
      if (volume && panes[1]) panes[1].setStretchFactor(VOLUME_PANE_STRETCH);
      studyPanes.forEach(function (pane) {
        if (panes[pane]) panes[pane].setStretchFactor(STUDY_PANE_STRETCH);
      });
      if (range) {
        try { chart.timeScale().setVisibleLogicalRange(range); } catch (_err) {}
      }
      renderControls();
    }

    function applyTimeZone() {
      chart.applyOptions({
        localization: {
          timeFormatter: function (time) {
            return formatTime(Number(time), prefs.timeZone);
          },
        },
        timeScale: {
          tickMarkFormatter: function (time, tickType) {
            return formatTick(time, tickType, prefs.timeZone);
          },
        },
      });
    }

    function redraw(keepRange) {
      if (!chart) return false;
      try {
        applyTimeZone();
        rebuild(keepRange);
        renderReadout(null);
        return true;
      } catch (err) {
        return report("draw", err);
      }
    }

    function onDisplayChange() {
      savePreferences();
      redraw(true);
    }

    function selectControl(name, labelText, list, value, onChange) {
      const label = el("label", "chart-control");
      const select = el("select", "chart-select");
      select.name = name;
      list.forEach(function (entry) {
        const option = el("option", "", entry.label);
        option.value = entry.value;
        select.append(option);
      });
      select.value = value;
      select.addEventListener("change", function () {
        if (!allowed(list, select.value)) return;
        onChange(select.value);
      });
      label.append(el("span", "chart-control-label", labelText), select);
      return { label: label, select: select };
    }

    function checkControl(name, labelText, value, onChange) {
      const label = el("label", "chart-control chart-control-check");
      const input = el("input");
      input.type = "checkbox";
      input.name = name;
      input.checked = !!value;
      input.addEventListener("change", function () {
        onChange(!!input.checked);
      });
      label.append(input, el("span", "chart-control-label", labelText));
      return { label: label, input: input };
    }

    function fullscreenSupported() {
      return !!(fullscreenTarget && typeof fullscreenTarget.requestFullscreen === "function" &&
        document.fullscreenEnabled !== false && typeof document.exitFullscreen === "function");
    }

    function updateFullscreenButton() {
      if (!controls.fullscreen) return;
      const active = document.fullscreenElement === fullscreenTarget;
      controls.fullscreen.textContent = active ? "Exit fullscreen" : "Fullscreen";
      controls.fullscreen.setAttribute("aria-pressed", active ? "true" : "false");
    }

    function toggleFullscreen() {
      setNote(controls.fullscreenNote, "");
      if (!fullscreenSupported()) {
        setNote(controls.fullscreenNote, "Fullscreen is not available in this browser.");
        return;
      }
      const refused = function (err) {
        setNote(controls.fullscreenNote, "The browser refused fullscreen" + (err ? ": " + messageOf(err) : "."));
      };
      try {
        const pending = document.fullscreenElement === fullscreenTarget
          ? document.exitFullscreen()
          : fullscreenTarget.requestFullscreen();
        if (pending && typeof pending.catch === "function") pending.catch(refused);
      } catch (err) {
        refused(err);
      }
    }

    function exportSupported() {
      return !!(chart && typeof chart.takeScreenshot === "function");
    }

    function exportImage() {
      setNote(controls.exportNote, "");
      if (!exportSupported()) {
        setNote(controls.exportNote, "Image export is not available in this browser.");
        return;
      }
      try {
        const canvas = chart.takeScreenshot();
        if (!canvas || typeof canvas.toDataURL !== "function") {
          setNote(controls.exportNote, "Image export is not available in this browser.");
          return;
        }
        const url = canvas.toDataURL("image/png");
        if (typeof url !== "string" || url.indexOf("data:image/png") !== 0) {
          setNote(controls.exportNote, "The browser refused the image export.");
          return;
        }
        const link = document.createElement("a");
        link.href = url;
        let name = opts.exportName;
        if (typeof name === "function") {
          try { name = name(); } catch (_err) { name = ""; }
        }
        link.download = exportFileName(name, Date.now());
        link.hidden = true;
        document.body.append(link);
        link.click();
        link.remove();
      } catch (err) {
        setNote(controls.exportNote, "The browser refused the image export: " + messageOf(err));
      }
    }

    function studyStatus(study) {
      if (study.error) return "Unavailable: " + study.error;
      if (study.drawError) return study.drawError;
      if (studyHidden()) return "Hidden: studies do not show on trade-derived bars.";
      const result = studyResults.get(study.id);
      return result && result.reason ? result.reason : "";
    }

    function removeStudy(study) {
      const position = studies.indexOf(study);
      if (position < 0) return;
      studies.splice(position, 1);
      if (study.dom && study.dom.item.parentNode) study.dom.item.parentNode.removeChild(study.dom.item);
      onDisplayChange();
    }

    function onStudyInput(study, param, input) {
      study.drafts[param.key] = input.value;
      const merged = Object.assign({}, study.params);
      Object.keys(study.drafts).forEach(function (key) {
        merged[key] = study.drafts[key];
      });
      const check = validateStudyParams(study.type, merged);
      if (!check.ok) {
        study.error = check.error;
        redraw(true);
        return;
      }
      study.error = "";
      study.drafts = {};
      study.params = check.params;
      onDisplayChange();
    }

    function buildStudyItem(study) {
      const spec = studySpec(study.type);
      const item = el("li", "chart-study");
      const label = el("span", "chart-study-label");
      label.title = spec.title;
      item.append(label);
      spec.params.forEach(function (param) {
        const field = el("label", "chart-study-field");
        const input = el("input", "chart-study-input");
        input.type = "number";
        input.name = study.id + "-" + param.key;
        input.min = String(param.min);
        input.max = String(param.max);
        input.step = String(param.step);
        input.value = formatParam(study.params[param.key]);
        input.inputMode = param.integer ? "numeric" : "decimal";
        input.addEventListener("input", function () {
          onStudyInput(study, param, input);
        });
        field.append(el("span", "chart-control-label", param.label), input);
        item.append(field);
      });
      const remove = el("button", "chart-button", "Remove");
      remove.type = "button";
      remove.addEventListener("click", function () {
        removeStudy(study);
      });
      const status = el("span", "chart-study-status");
      item.append(remove, status);
      study.dom = { item: item, label: label, remove: remove, status: status };
      return item;
    }

    function renderStudies() {
      if (!controls.studyList) return;
      studies.forEach(function (study) {
        if (!study.dom) controls.studyList.append(buildStudyItem(study));
        study.dom.label.textContent = studyLabel(study.type, study.params);
        study.dom.remove.setAttribute("aria-label", "Remove " + studyLabel(study.type, study.params));
        const status = studyStatus(study);
        study.dom.status.textContent = status;
        study.dom.status.hidden = !status;
      });
      controls.studyList.hidden = studies.length === 0;
      controls.studyAdd.disabled = studies.length >= MAX_STUDIES;
      controls.studySelect.disabled = studies.length >= MAX_STUDIES;
      setNote(controls.studyNote, studies.length >= MAX_STUDIES ? "At most " + MAX_STUDIES + " studies." : "");
    }

    function renderControls() {
      if (!toolbar || !controls.type) return;
      controls.type.select.value = prefs.chartType;
      controls.scale.select.value = prefs.scale;
      controls.time.select.value = prefs.timeZone;
      controls.lastLine.input.checked = !!prefs.lastPriceLine;
      controls.lastLabel.input.checked = !!prefs.lastPriceLabel;
      setNote(controls.scaleNote, effectiveScale.reason);
      renderStudies();
    }

    function buildToolbar() {
      if (!toolbar) return;
      toolbar.replaceChildren();
      const row = el("div", "chart-toolbar-row");
      controls.type = selectControl("chart-type", "Type", CHART_TYPES, prefs.chartType, function (value) {
        prefs.chartType = value;
        onDisplayChange();
      });
      controls.scale = selectControl("chart-scale", "Scale", SCALES, prefs.scale, function (value) {
        prefs.scale = value;
        onDisplayChange();
      });
      controls.time = selectControl("chart-time-zone", "Time", TIME_ZONES, prefs.timeZone, function (value) {
        prefs.timeZone = value;
        onDisplayChange();
      });
      controls.lastLine = checkControl("chart-last-price-line", "Last price line", prefs.lastPriceLine, function (value) {
        prefs.lastPriceLine = value;
        onDisplayChange();
      });
      controls.lastLabel = checkControl("chart-last-price-label", "Last price label", prefs.lastPriceLabel, function (value) {
        prefs.lastPriceLabel = value;
        onDisplayChange();
      });
      const fullscreenGroup = el("span", "chart-control-group");
      controls.fullscreen = el("button", "chart-button", "Fullscreen");
      controls.fullscreen.type = "button";
      controls.fullscreen.setAttribute("aria-pressed", "false");
      controls.fullscreenNote = el("span", "chart-control-note");
      controls.fullscreenNote.setAttribute("role", "status");
      controls.fullscreen.addEventListener("click", toggleFullscreen);
      if (!fullscreenSupported()) {
        controls.fullscreen.disabled = true;
        controls.fullscreen.title = "Fullscreen is not available in this browser.";
      }
      fullscreenGroup.append(controls.fullscreen, controls.fullscreenNote);
      const exportGroup = el("span", "chart-control-group");
      controls.exportButton = el("button", "chart-button", "Export image");
      controls.exportButton.type = "button";
      controls.exportNote = el("span", "chart-control-note");
      controls.exportNote.setAttribute("role", "status");
      controls.exportButton.addEventListener("click", exportImage);
      if (!exportSupported()) {
        controls.exportButton.disabled = true;
        controls.exportButton.title = "Image export is not available in this browser.";
      }
      exportGroup.append(controls.exportButton, controls.exportNote);
      row.append(controls.type.label, controls.scale.label, controls.time.label, controls.lastLine.label,
        controls.lastLabel.label, fullscreenGroup, exportGroup);

      const studyRow = el("div", "chart-toolbar-row");
      const studyLabelEl = el("label", "chart-control");
      controls.studySelect = el("select", "chart-select");
      controls.studySelect.name = "chart-study";
      STUDY_ORDER.forEach(function (type) {
        const option = el("option", "", STUDY_SPECS[type].title + " (" + STUDY_SPECS[type].name + ")");
        option.value = type;
        controls.studySelect.append(option);
      });
      studyLabelEl.append(el("span", "chart-control-label", "Study"), controls.studySelect);
      controls.studyAdd = el("button", "chart-button", "Add study");
      controls.studyAdd.type = "button";
      controls.studyAdd.addEventListener("click", function () {
        const type = controls.studySelect.value;
        if (!studySpec(type) || studies.length >= MAX_STUDIES) return;
        studies.push(newStudy(type, defaultStudyParams(type)));
        onDisplayChange();
      });
      controls.studyNote = el("span", "chart-control-note");
      studyRow.append(studyLabelEl, controls.studyAdd, controls.studyNote);
      controls.studyList = el("ul", "chart-study-list");
      controls.scaleNote = el("p", "chart-toolbar-note");
      controls.scaleNote.setAttribute("role", "status");
      controls.storageNote = el("p", "chart-toolbar-note");
      controls.storageNote.setAttribute("role", "status");
      toolbar.append(row, studyRow, controls.studyList, controls.scaleNote, controls.storageNote);
      [controls.fullscreenNote, controls.exportNote, controls.studyNote, controls.scaleNote, controls.storageNote].forEach(function (node) {
        node.hidden = true;
      });
      controls.studyList.hidden = true;
      toolbar.hidden = false;
      fullscreenListener = function () {
        updateFullscreenButton();
      };
      document.addEventListener("fullscreenchange", fullscreenListener);
    }

    function setup() {
      try {
        if (!container) throw new Error("The chart container is missing.");
        lib = window.LightweightCharts;
        if (!lib || typeof lib.createChart !== "function" || typeof lib.createSeriesMarkers !== "function") {
          throw new Error("The chart library did not load.");
        }
        mount = el("div", "chart-mount");
        readoutEl = el("div", "chart-readout");
        readoutEl.hidden = true;
        readoutEl.setAttribute("aria-live", "off");
        container.append(mount, readoutEl);
        chart = lib.createChart(mount, Object.assign({}, themeOptions(isDark()), {
          crosshair: { mode: lib.CrosshairMode.Normal },
        }));
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
        try {
          buildToolbar();
        } catch (_err) {
          if (toolbar) {
            toolbar.replaceChildren();
            toolbar.hidden = true;
          }
        }
        applyTimeZone();
        rebuild(false);
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
          index = buildBarIndex(payload.candles || [], payload.source);
          ok = true;
          applyTimeZone();
          rebuild(false);
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
          let previous = null;
          if (keepRange && index.bars.length) {
            try { previous = chart.timeScale().getVisibleRange(); } catch (_err) { previous = null; }
          }
          seriesClass = p.sourceClass || (isTradeDerivedSource(p.source) ? "synthetic" : "market");
          formingTime = Number(p.formingTime) || 0;
          index = buildBarIndex(mergeBars([], p.candles || []), seriesClass === "synthetic" ? "trades" : p.source);
          ok = true;
          applyTimeZone();
          rebuild(false);
          renderReadout(null);
          if (!index.bars.length) return true;
          const first = index.times[0];
          const last = index.times[index.times.length - 1];
          if (previous && Number(previous.from) <= last && Number(previous.to) >= first) {
            chart.timeScale().setVisibleRange(previous);
          } else {
            chart.timeScale().fitContent();
          }
          return true;
        } catch (err) {
          index = buildBarIndex([], "");
          renderReadout(null);
          return report("draw", err);
        }
      },
      prependBars: function (list) {
        if (!chart) return 0;
        try {
          const before = index.bars.length;
          const merged = mergeBars(index.bars, list || []);
          const added = merged.length - before;
          if (added <= 0) return 0;
          let range = null;
          try { range = chart.timeScale().getVisibleLogicalRange(); } catch (_err) { range = null; }
          index = buildBarIndex(merged, seriesSourceLabel());
          rebuild(false);
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
        try {
          formingTime = Number(nextFormingTime) || 0;
          const incoming = list || [];
          if (incoming.length) {
            index = buildBarIndex(mergeBars(index.bars, incoming), seriesSourceLabel());
            rebuild(true);
          }
          renderReadout(null);
          return true;
        } catch (err) {
          return report("draw", err);
        }
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
          markerDefs = sortMarkers(list);
          markersApi.setMarkers(markerDefs);
          return true;
        } catch (err) {
          return report("draw", err);
        }
      },
      setPriceLines: function (group, lines) {
        if (!chart) return false;
        try {
          detachLines(group);
          const defs = (lines || []).map(function (line) {
            return { price: line.price, color: line.color, title: line.title, style: line.style };
          });
          lineDefs.set(group, defs);
          attachLines(group);
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
          markerDefs = [];
          lineDefs.clear();
          index = buildBarIndex([], "");
          seriesClass = "";
          formingTime = 0;
          rebuild(false);
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
