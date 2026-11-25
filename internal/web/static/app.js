/* FFMUC Starlink Manager — lightweight vanilla JS. No external dependencies. */
(function () {
  "use strict";

  function fmtBytes(bps) {
    if (!isFinite(bps) || bps <= 0) return "0 bps";
    var units = ["bps", "Kbps", "Mbps", "Gbps"];
    var i = 0;
    while (bps >= 1000 && i < units.length - 1) { bps /= 1000; i++; }
    return bps.toFixed(bps >= 100 ? 0 : 1) + " " + units[i];
  }

  function fmtAgo(s) {
    if (!isFinite(s) || s < 0) return "never";
    if (s < 60) return Math.round(s) + "s ago";
    if (s < 3600) return Math.round(s / 60) + "m ago";
    return Math.round(s / 3600) + "h ago";
  }

  function esc(s) {
    var d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
  }

  function restoreSummary(summary) {
    if (summary && summary.dataset.baseText != null) {
      summary.textContent = summary.dataset.baseText;
      delete summary.dataset.baseText;
    }
  }

  /* ---------- SVG line chart ---------- */
  function drawChart(svg, series, opts) {
    var ns = "http://www.w3.org/2000/svg";
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    svg.onpointermove = svg.onpointerleave = svg.onkeydown = svg.onfocus = svg.onblur = null;
    var tooltip = svg.parentElement.querySelector(".chart-tooltip");
    if (tooltip) tooltip.hidden = true;
    var summary = svg.parentElement.querySelector(".chart-summary");
    // A redraw (resize, theme, new window) drops the cursor, so put back a
    // summary that an inspected sample replaced.
    restoreSummary(summary);

    var W = Math.max(240, svg.clientWidth || 480), H = 180, padL = 66, padB = 24, padT = 12, padR = 12;
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    svg.style.aspectRatio = W + " / " + H;
    function node(tag, attrs, parent) {
      var el = document.createElementNS(ns, tag);
      Object.keys(attrs).forEach(function (k) { el.setAttribute(k, attrs[k]); });
      (parent || svg).appendChild(el);
      return el;
    }
    if (!series || !series.length) {
      node("text", { x: W / 2, y: H / 2, "text-anchor": "middle" }).textContent = opts.emptyText || "No samples in this window";
      return;
    }

    var now = Math.floor(Date.now() / 1000);
    var tMin = opts.tMin != null ? opts.tMin : series[0].t;
    var tMax = Math.max(now, series[series.length - 1].t);
    if (tMax === tMin) tMax = tMin + 1;

    var vMin = Infinity, vMax = -Infinity;
    series.forEach(function (p) { vMin = Math.min(vMin, p.v); vMax = Math.max(vMax, p.v); });
    var span = vMax - vMin || Math.max(vMax * 0.1, 1);
    vMin = Math.max(0, vMin - span * 0.08);
    vMax += span * 0.12;
    if (opts.binary) { vMin = 0; vMax = 1; }

    function x(t) { return padL + (t - tMin) / (tMax - tMin) * (W - padL - padR); }
    function y(v) { return padT + (vMax - v) / (vMax - vMin) * (H - padT - padB); }

    var ticks = opts.binary ? 1 : 3;
    for (var g = 0; g <= ticks; g++) {
      var v = vMin + (vMax - vMin) * g / ticks;
      var gy = y(v);
      node("line", { x1: padL, x2: W - padR, y1: gy, y2: gy, class: "gridline" });
      node("text", { x: padL - 8, y: gy + 3, "text-anchor": "end" }).textContent = opts.binary ? (v ? "Yes" : "No") : opts.fmt(v);
    }
    /* x labels: tMin and tMax */
    [[x(tMin), fmtAgo(now - tMin)], [x(tMax), "now"]].forEach(function (p) {
      node("text", { x: p[0], y: H - 4, "text-anchor": p[1] === "now" ? "end" : "start" }).textContent = p[1];
    });

    var d = "";
    series.forEach(function (p, i) {
      var px = x(p.t), py = y(p.v);
      if (!i) { d = "M" + px + " " + py; return; }
      var prev = series[i - 1];
      if (opts.binary) {
        d += "H" + px + "V" + py;
      } else {
        // Horizontal control tangents soften the line without overshooting values.
        var mid = (x(prev.t) + px) / 2;
        d += "C" + mid + " " + y(prev.v) + " " + mid + " " + py + " " + px + " " + py;
      }
    });
    var gradientID = "fill-" + svg.dataset.chart;
    var gradient = node("linearGradient", { id: gradientID, x1: "0", y1: "0", x2: "0", y2: "1" }, node("defs", {}));
    node("stop", { offset: "0%", "stop-color": opts.color, "stop-opacity": ".22" }, gradient);
    node("stop", { offset: "100%", "stop-color": opts.color, "stop-opacity": ".01" }, gradient);
    node("path", { d: d + "L" + x(series[series.length - 1].t) + " " + (H - padB) + "L" + x(series[0].t) + " " + (H - padB) + "Z", fill: "url(#" + gradientID + ")" });
    node("path", { d: d, fill: "none", stroke: opts.color, "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round", "vector-effect": "non-scaling-stroke" });
    if (series.length === 1) node("circle", { cx: x(series[0].t), cy: y(series[0].v), r: 3, fill: opts.color });

    var cursor = node("g", { visibility: "hidden", "aria-hidden": "true" });
    var cross = node("line", { y1: padT, y2: H - padB, class: "crosshair" }, cursor);
    var dot = node("circle", { r: 4, fill: opts.color, "stroke-width": 2 }, cursor);
    // var() is not valid in SVG presentation attributes; use a style.
    dot.style.stroke = "var(--bg-card)";
    var selected = series.length - 1;
    function show(index, announce) {
      selected = Math.max(0, Math.min(index, series.length - 1));
      var p = series[selected], px = x(p.t);
      cursor.setAttribute("visibility", "visible");
      cross.setAttribute("x1", px); cross.setAttribute("x2", px);
      dot.setAttribute("cx", px); dot.setAttribute("cy", y(p.v));
      var text = new Date(p.t * 1000).toLocaleString() + " · " + (opts.binary ? (p.v ? "Yes" : "No") : opts.valueFmt(p.v));
      if (tooltip) { tooltip.textContent = text; tooltip.hidden = false; }
      if (announce && summary) {
        if (summary.dataset.baseText == null) summary.dataset.baseText = summary.textContent;
        summary.textContent = text;
      }
    }
    function hide() {
      cursor.setAttribute("visibility", "hidden");
      if (tooltip) tooltip.hidden = true;
      restoreSummary(summary);
    }
    svg.onpointermove = function (ev) {
      var point = svg.createSVGPoint();
      point.x = ev.clientX; point.y = ev.clientY;
      var px = point.matrixTransform(svg.getScreenCTM().inverse()).x;
      var target = tMin + (px - padL) / (W - padL - padR) * (tMax - tMin);
      var low = 0, high = series.length - 1;
      while (low < high) {
        var mid = Math.floor((low + high) / 2);
        if (series[mid].t < target) low = mid + 1;
        else high = mid;
      }
      if (low > 0 && target - series[low - 1].t < series[low].t - target) low--;
      show(low, false);
    };
    svg.onpointerleave = hide;
    svg.onfocus = function () { show(selected, true); };
    svg.onblur = hide;
    svg.onkeydown = function (ev) {
      if (ev.key === "Escape") { hide(); return; }
      if (["ArrowLeft", "ArrowRight", "Home", "End"].indexOf(ev.key) < 0) return;
      ev.preventDefault();
      show(ev.key === "Home" ? 0 : ev.key === "End" ? series.length - 1 : selected + (ev.key === "ArrowRight" ? 1 : -1), true);
    };
  }

  /* summary formatters receive plain numbers (the plotted values) */
  function fmtMs(v) { return v.toFixed(0) + " ms"; }
  function fmtPct(v) { return v.toFixed(1) + " %"; }

  var CHARTS = {
    ping:     { title: "Ping latency (ms)", color: "#79b8ff", lightColor: "#286ec4", valueFmt: fmtMs, fmt: function (v) { return v.toFixed(0); },
                pick: function (s) { return { t: s.ts, v: s.pingMs }; }, summary: function (a) { return avgSum(a, fmtMs); } },
    /* dropRate is a 0..1 fraction; plot it as a percentage */
    drop:     { title: "Ping drop rate (%)", color: "#f28b99", lightColor: "#b42d40", valueFmt: fmtPct, fmt: function (v) { return v.toFixed(1); },
                pick: function (s) { return { t: s.ts, v: s.dropRate * 100 }; }, summary: function (a) { return avgSum(a, fmtPct); } },
    downlink: { title: "Downlink throughput", color: "#73e0c1", lightColor: "#087b62", valueFmt: fmtBytes, fmt: fmtBytes,
                pick: function (s) { return { t: s.ts, v: s.downBps }; }, summary: function (a) { return avgSum(a, fmtBytes); } },
    uplink:   { title: "Uplink throughput", color: "#b8a4f4", lightColor: "#7452b4", valueFmt: fmtBytes, fmt: fmtBytes,
                pick: function (s) { return { t: s.ts, v: s.upBps }; }, summary: function (a) { return avgSum(a, fmtBytes); } },
    obstruction: { title: "Obstruction", color: "#f28b99", lightColor: "#b42d40", binary: true, fmt: function (v) { return v.toFixed(0); },
                pick: function (s) { return { t: s.ts, v: s.obstructed ? 1 : 0 }; },
                summary: function (a) { return fractionSummary(a, "of samples obstructed"); } },
    scheduled: { title: "Scheduled (boresight) windows", color: "#f2c574", lightColor: "#8b580c", binary: true, fmt: function (v) { return v.toFixed(0); },
                pick: function (s) { return { t: s.ts, v: s.scheduled ? 1 : 0 }; },
                summary: function (a) { return fractionSummary(a, "of samples scheduled"); } }
  };

  function fractionSummary(arr, label) {
    if (!arr.length) return "no samples";
    var n = 0;
    arr.forEach(function (v) { n += v; });
    return (100 * n / arr.length).toFixed(1) + "% " + label;
  }

  function avgSum(arr, fmt) {
    if (!arr.length) return "no samples";
    var s = 0;
    arr.forEach(function (v) { s += v; });
    return "latest " + fmt(arr[arr.length - 1]) + " · avg " + fmt(s / arr.length);
  }

  /* ---------- helpers ---------- */
  /* Resolve to {ok, status, body}; non-JSON replies (auth redirects, proxy
     errors, plain-text 403s) and network failures become {error: ...}. */
  function toResult(r) {
    return r.text().then(function (t) {
      var body;
      try { body = JSON.parse(t); } catch (e) { body = { error: (t || r.statusText || "HTTP " + r.status).trim().slice(0, 200) }; }
      if (body == null || typeof body !== "object") body = { error: "unexpected response" };
      return { ok: r.ok, status: r.status, body: body };
    });
  }
  function netError(e) { return { ok: false, status: 0, body: { error: "network error: " + (e && e.message || e) } }; }

  function fetchJSON(url, opts) {
    return fetch(url, Object.assign({ headers: { "Accept": "application/json" } }, opts || {}))
      .then(toResult, netError);
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify(body)
    }).then(toResult, netError);
  }

  function currentWindow() {
    var sel = document.querySelector(".btn-selected[data-window]");
    return sel ? sel.getAttribute("data-window") : "1h";
  }

  var historyRequest = 0;
  var chartCache = {};
  function renderHistoryChart(key, series, tMin) {
    var svg = document.querySelector('svg[data-chart="' + key + '"]');
    var chart = CHARTS[key];
    if (svg) drawChart(svg, series, {
      color: document.documentElement.dataset.theme === "light" ? chart.lightColor : chart.color,
      fmt: chart.fmt, valueFmt: chart.valueFmt, binary: chart.binary, tMin: tMin
    });
  }

  function loadHistory(deviceId, win) {
    var request = ++historyRequest;
    var names = Object.keys(CHARTS);
    chartCache = {};
    names.forEach(function (key) {
      var svg = document.querySelector('svg[data-chart="' + key + '"]');
      var sum = document.querySelector('[data-chart-summary="' + key + '"]');
      if (svg) { svg.setAttribute("aria-busy", "true"); drawChart(svg, [], { emptyText: "Loading history…" }); }
      if (sum) sum.textContent = "Loading history…";
    });
    fetchJSON("/api/device/" + deviceId + "/history?window=" + encodeURIComponent(win))
      .then(function (r) {
        if (request !== historyRequest) return;
        var samples = r.body.samples || [];
        names.forEach(function (key) {
          var svg = document.querySelector('svg[data-chart="' + key + '"]');
          var sum = document.querySelector('[data-chart-summary="' + key + '"]');
          if (svg) svg.setAttribute("aria-busy", "false");
          if (!r.ok) {
            if (svg) drawChart(svg, [], { emptyText: "History unavailable" });
            if (sum) sum.textContent = "Could not load history. Select a window to retry.";
            return;
          }
          var series = samples.map(CHARTS[key].pick).filter(function (p) { return p.v != null && isFinite(p.v); });
          chartCache[key] = { series: series, tMin: r.body.tMin };
          renderHistoryChart(key, series, r.body.tMin);
          if (sum) sum.textContent = CHARTS[key].summary(series.map(function (p) { return p.v; })).replace("{v}", "");
        });
      });
  }

  function loadSummary() {
    fetchJSON("/api/summary").then(function (r) {
      var info = document.querySelector("[data-poll-info]");
      if (!r.ok) {
        if (info) info.textContent = "Refresh unavailable · showing last known status";
        return;
      }
      if (info) {
        var d = r.body;
        info.textContent = (d.sessionDead ? "Session invalid · " : "") +
          "Status refreshed " + fmtAgo((Date.now() / 1000) - d.updatedAt) + " · every 30s";
      }
      var counts = { total: 0, online: 0, degraded: 0, offline: 0 };
      (r.body.devices || []).forEach(function (dev) {
        var healthClass = dev.onlineClass;
        var healthLabel = dev.onlineLabel;
        if (healthClass === "pill-ok" && ((dev.alerts || []).length || dev.obstructionClass === "pill-warn" || dev.obstructionClass === "pill-error")) {
          healthClass = "pill-warn"; healthLabel = "degraded";
        }
        counts.total++;
        counts[healthClass === "pill-ok" ? "online" : healthClass === "pill-warn" ? "degraded" : "offline"]++;
        var root = Array.from(document.querySelectorAll(".device-card")).find(function (card) { return card.dataset.deviceId === dev.id; });
        if (!root) return;
        if (dev.onlineLabel != null) {
          var pill = root.querySelector('[data-field="online"]');
          if (pill) { pill.textContent = healthLabel; pill.className = "pill " + healthClass; }
        }
        ["state", "snr", "obstruction", "versions", "alerts", "updated"].forEach(function (f) {
          if (dev[f] == null && f !== "alerts") return;
          var el = root.querySelector('[data-field="' + f + '"]');
          if (!el) return;
          if (f === "alerts") {
            el.innerHTML = (dev.alerts || []).map(function (a) {
              return '<span class="pill pill-warn">' + esc(a) + "</span>";
            }).join("");
          } else if (f === "obstruction") {
            el.innerHTML = '<span class="pill ' + (dev.obstructionClass || "pill-muted") + '">' + esc(dev.obstruction) + "</span>";
          } else {
            el.textContent = dev[f];
          }
        });
      });
      // Count the current roster once all devices are tallied; the card
      // inventory is a server-rendered snapshot.
      var grid = document.getElementById("device-grid");
      var ids = (r.body.devices || []).map(function (dev) { return dev.id; });
      if (grid) {
        var cards = grid.querySelectorAll(".device-card");
        var sameRoster = cards.length === ids.length && Array.from(cards).every(function (card) { return ids.indexOf(card.dataset.deviceId) >= 0; });
        if (sameRoster) {
          Object.keys(counts).forEach(function (key) {
            var el = document.querySelector('[data-count="' + key + '"]');
            if (el) el.textContent = counts[key];
          });
        } else if (info) info.textContent = "Device roster changed · reload to update the inventory";
      } else if (ids.length && document.querySelector(".empty-card") && info) {
        info.textContent = "Devices found · reload to show them";
      }
    });
  }

  function loadClients(deviceId) {
    var tbody = document.querySelector("#clients-table tbody");
    if (!tbody) return;
    fetchJSON("/api/device/" + deviceId + "/clients").then(function (r) {
      if (!r.ok) {
        tbody.innerHTML = '<tr><td colspan="5" class="muted">' + esc(r.body.error || "failed to load clients") + "</td></tr>";
        return;
      }
      var c = r.body.clients || [];
      if (!c.length) {
        tbody.innerHTML = '<tr><td colspan="5" class="muted">no clients connected</td></tr>';
        return;
      }
      tbody.innerHTML = c.map(function (cl) {
        return "<tr><td>" + esc(cl.name || "unknown") + "</td><td><code>" + esc(cl.mac || "") + "</code></td>" +
          "<td>" + esc(cl.band || "") + "</td><td>" + esc(cl.ip || "") + "</td><td>" + esc(cl.dBm || "") + "</td></tr>";
      }).join("");
    });
  }

  /* ---------- action buttons ---------- */
  var CONFIRM_TEXT = {
    reboot: "Reboot this device? It will be offline for several minutes.",
    stow: "Stow the dish (flat, safe position)?",
    unstow: "Unstow the dish (activate motors to track satellites)?",
    inhibit_rf: "Inhibit RF? The dish will stop transmitting immediately.",
    allow_rf: "Allow RF again? The dish resumes normal transmission.",
    power_save: "Configure scheduled power save?"
  };

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("button[data-action]");
    if (!btn) return;
    var id = btn.getAttribute("data-device-id");
    var action = btn.getAttribute("data-action");

    if (action === "refresh_clients") { loadClients(id); return; }

    if (action === "power_save") {
      ev.preventDefault();
      var start = prompt("Power save start: minutes past midnight UTC (0 = 00:00 UTC). The dish schedule clock is UTC, not local time:", "0");
      if (start === null) return;
      var dur = prompt("Duration in minutes:", "480");
      if (dur === null) return;
      var enable = confirm("Enable scheduled power save (OK) or disable it (Cancel)?\nOK = enable with start " + start + " minutes past midnight UTC for " + dur + " minutes\nCancel = disable power save");
      if (!enable && !confirm("Disable power save now?")) return;
      btn.disabled = true;
      postJSON("/api/device/" + id + "/action", {
        action: "power_save", enable: enable,
        startMinutes: parseInt(start, 10) || 0,
        durationMinutes: parseInt(dur, 10) || 0
      }).then(function (r) { showOutcome(r); btn.disabled = false; });
      return;
    }

    var msg = CONFIRM_TEXT[action];
    if (!msg) return;
    if (!confirm(msg)) return;
    ev.preventDefault();
    btn.disabled = true;
    postJSON("/api/device/" + id + "/action", { action: action })
      .then(function (r) { showOutcome(r); btn.disabled = false; });
  });

  function showOutcome(r) {
    var box = document.getElementById("action-outcome");
    if (!box) return;
    box.hidden = false;
    var ok = r.ok && r.body.ok;
    box.className = "outcome " + (ok ? "ok" : "err");
    box.textContent = ok ? "✓ " + (r.body.message || "done")
      : "✗ " + (r.body.error || r.body.message || "request failed (HTTP " + r.status + ")");
    if (ok) setTimeout(loadSummary, 1500);
  }

  /* ---------- init per page ---------- */
  document.addEventListener("DOMContentLoaded", function () {
    var themeButton = document.querySelector("[data-theme-toggle]");
    function updateThemeButton() {
      var light = document.documentElement.dataset.theme === "light";
      themeButton.textContent = light ? "Dark mode" : "Light mode";
      themeButton.setAttribute("aria-label", "Switch to " + (light ? "dark" : "light") + " theme");
      themeButton.setAttribute("aria-pressed", String(light));
    }
    if (themeButton) {
      themeButton.hidden = false;
      updateThemeButton();
      themeButton.addEventListener("click", function () {
        var theme = document.documentElement.dataset.theme === "light" ? "dark" : "light";
        document.documentElement.dataset.theme = theme;
        try { localStorage.setItem("ffmuc-theme", theme); } catch (_) {}
        updateThemeButton();
        Object.keys(chartCache).forEach(function (key) { renderHistoryChart(key, chartCache[key].series, chartCache[key].tMin); });
      });
    }
    var charts = document.querySelectorAll("svg[data-chart]");
    if (typeof ResizeObserver !== "undefined") {
      var widths = new WeakMap();
      var observer = new ResizeObserver(function (entries) {
        entries.forEach(function (entry) {
          var svg = entry.target, width = Math.round(entry.contentRect.width);
          if (widths.get(svg) === width) return;
          widths.set(svg, width);
          var cached = chartCache[svg.dataset.chart];
          if (cached) renderHistoryChart(svg.dataset.chart, cached.series, cached.tMin);
        });
      });
      charts.forEach(function (svg) { observer.observe(svg); });
    }
    var deviceId = charts.length ? charts[0].getAttribute("data-device-id") : null;
    if (deviceId) {
      loadHistory(deviceId, currentWindow());
      document.querySelectorAll("button[data-window]").forEach(function (b) {
        b.addEventListener("click", function () {
          document.querySelectorAll("button[data-window]").forEach(function (x) { x.classList.remove("btn-selected"); x.setAttribute("aria-pressed", "false"); });
          b.classList.add("btn-selected");
          b.setAttribute("aria-pressed", "true");
          loadHistory(deviceId, b.getAttribute("data-window"));
        });
      });
    }
    if (document.getElementById("clients-table")) {
      loadClients(document.querySelector("#clients-table").getAttribute("data-device-id") ||
        document.querySelector("button[data-action=refresh_clients]").getAttribute("data-device-id"));
    }
    loadSummary();
    setInterval(loadSummary, 30000);
  });
})();
