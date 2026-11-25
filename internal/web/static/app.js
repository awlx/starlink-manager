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
    svg.onpointerdown = svg.onpointermove = svg.onpointerleave = svg.onkeydown = svg.onfocus = svg.onblur = null;
    var tooltip = svg.parentElement.querySelector(".chart-tooltip");
    if (tooltip) tooltip.hidden = true;
    var summary = svg.parentElement.querySelector(".chart-summary");
    // A redraw (resize, theme, new window) drops the cursor, so put back a
    // summary that an inspected sample replaced.
    restoreSummary(summary);

    var W = Math.max(240, svg.clientWidth || 480), H = series && series.length ? 180 : 70, padL = 66, padB = 24, padT = 12, padR = 12;
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
    var paths = [];
    var ends = [];
    var intervals = series.slice(1).map(function (p, i) { return p.t - series[i].t; }).sort(function (a, b) { return a - b; });
    var gap = series.length >= 1000 ? Math.max(120, (intervals[Math.floor(intervals.length / 2)] || 0) * 3) : 120;
    series.forEach(function (p, i) {
      var px = x(p.t), py = y(p.v);
      if (!i) { paths.push("M" + px + " " + py); ends.push(px); return; }
      var prev = series[i - 1];
      // Respect coarser downsampled windows without bridging offline gaps.
      if (p.t - prev.t > gap) { paths.push("M" + px + " " + py); ends.push(px); return; }
      ends[ends.length - 1] = px;
      var cur = paths[paths.length - 1];
      if (opts.binary) {
        paths[paths.length - 1] = cur + "H" + px + "V" + py;
      } else {
        // Horizontal control tangents soften the line without overshooting values.
        var mid = (x(prev.t) + px) / 2;
        paths[paths.length - 1] = cur + "C" + mid + " " + y(prev.v) + " " + mid + " " + py + " " + px + " " + py;
      }
    });
    d = paths.join(" ");
    var gradientID = "fill-" + svg.dataset.chart;
    var gradient = node("linearGradient", { id: gradientID, x1: "0", y1: "0", x2: "0", y2: "1" }, node("defs", {}));
    node("stop", { offset: "0%", "stop-color": opts.color, "stop-opacity": ".22" }, gradient);
    node("stop", { offset: "100%", "stop-color": opts.color, "stop-opacity": ".01" }, gradient);
    node("path", { d: paths.map(function (seg, i) {
      // Close each continuous segment to the baseline so offline gaps do
      // not get an area fill bridging across them.
      var m = seg.match(/^M(\S+) (\S+)/);
      if (!m) return "";
      return seg + "L" + ends[i] + " " + (H - padB) + "L" + m[1] + " " + (H - padB) + "Z";
    }).join(" "), fill: "url(#" + gradientID + ")" });
    node("path", { d: d, fill: "none", stroke: opts.color, "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round", "vector-effect": "non-scaling-stroke" });
    if (series.length === 1) node("circle", { cx: x(series[0].t), cy: y(series[0].v), r: 3, fill: opts.color });

    var cursor = node("g", { visibility: "hidden", "aria-hidden": "true" });
    var cross = node("line", { y1: padT, y2: H - padB, class: "crosshair" }, cursor);
    var dot = node("circle", { r: 4, fill: opts.color, "stroke-width": 2 }, cursor);
    // var() is not valid in SVG presentation attributes; use a style.
    dot.style.stroke = "var(--bg-card)";
    var selected = series.findIndex(function (p) { return p.t === Number(svg.dataset.selectedTime); });
    if (selected < 0) selected = series.length - 1;
    function show(index, announce) {
      selected = Math.max(0, Math.min(index, series.length - 1));
      var p = series[selected], px = x(p.t);
      svg.dataset.selectedTime = p.t;
      cursor.setAttribute("visibility", "visible");
      cross.setAttribute("x1", px); cross.setAttribute("x2", px);
      dot.setAttribute("cx", px); dot.setAttribute("cy", y(p.v));
      var text = new Date(p.t * 1000).toISOString().replace("T", " ").replace(".000Z", " UTC") + " · " + (opts.binary ? (p.v ? "Yes" : "No") : opts.valueFmt(p.v));
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
    function inspect(ev) {
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
      show(low, ev.pointerType === "touch");
    }
    svg.onpointerdown = svg.onpointermove = inspect;
    svg.onpointerleave = function (ev) {
      if (ev.pointerType !== "touch" && document.activeElement !== svg) hide();
    };
    svg.onfocus = function () { show(selected, true); };
    svg.onblur = hide;
    svg.onkeydown = function (ev) {
      if (ev.key === "Escape") { hide(); return; }
      if (["ArrowLeft", "ArrowRight", "Home", "End"].indexOf(ev.key) < 0) return;
      ev.preventDefault();
      show(ev.key === "Home" ? 0 : ev.key === "End" ? series.length - 1 : selected + (ev.key === "ArrowRight" ? 1 : -1), true);
    };
    if (document.activeElement === svg) show(selected, true);
  }

  /* summary formatters receive plain numbers (the plotted values) */
  function fmtMs(v) { return v.toFixed(0) + " ms"; }
  function fmtPct(v) { return v.toFixed(1) + " %"; }

  var CHARTS = {
    snr: { title: "SNR (reported samples)", color: "#79b8ff", lightColor: "#286ec4", valueFmt: function (v) { return v.toFixed(1); }, fmt: function (v) { return v.toFixed(1); },
      summary: function (a) { return avgSum(a, function (v) { return v.toFixed(1); }); } },
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
    scheduled: { title: "Scheduled (reported flag)", color: "#f2c574", lightColor: "#8b580c", binary: true, fmt: function (v) { return v.toFixed(0); },
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
      .then(toResult).catch(netError);
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify(body)
    }).then(toResult).catch(netError);
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
          var series;
          if (svg && svg.hasAttribute("data-series")) {
            var duration = parseFloat(win) * (win.indexOf("h") >= 0 ? 3600 : 60);
            series = JSON.parse(svg.dataset.series || "null") || [];
            series = series.filter(function (p) { return p.t >= Date.now() / 1000 - duration; });
          } else {
            series = samples.map(CHARTS[key].pick).filter(function (p) { return p.v != null && isFinite(p.v); });
          }
          chartCache[key] = { series: series, tMin: r.body.tMin };
          if (sum) {
            delete sum.dataset.baseText;
            sum.textContent = series.length ? CHARTS[key].summary(series.map(function (p) { return p.v; })) : "No history collected in this window. Try a longer window or wait for the device to reconnect.";
          }
          renderHistoryChart(key, series, r.body.tMin);
        });
      });
  }

  function loadSummary() {
    // Reuse server-rendered presentation so roster errors, retirement and
    // diagnostics recover together, without extending public API shapes.
    fetch(location.pathname, { headers: { "Accept": "text/html" } }).then(function (r) {
      if (!r.ok || r.redirected) throw new Error("refresh unavailable");
      return r.text();
    }).then(function (html) {
      var next = new DOMParser().parseFromString(html, "text/html");
      if (!next.querySelector("[data-refresh=account-banners]")) throw new Error("session changed");
      var info = document.querySelector("[data-poll-info]");
      var error = document.getElementById("refresh-error");
      if (error) error.hidden = true;
      if (info) info.textContent = "Status refreshed just now · every 30s";
      document.querySelectorAll("[data-refresh]").forEach(function (el) {
        var fresh = next.querySelector('[data-refresh="' + el.dataset.refresh + '"]');
        if (fresh && fresh.innerHTML !== el.innerHTML) el.innerHTML = fresh.innerHTML;
      });
      document.querySelectorAll("svg[data-series]").forEach(function (svg) {
        var fresh = next.querySelector('svg[data-chart="' + svg.dataset.chart + '"]');
        if (fresh) svg.dataset.series = fresh.dataset.series;
      });
      var cards = Array.from(document.querySelectorAll("#device-grid .device-card"));
      var freshCards = Array.from(next.querySelectorAll("#device-grid .device-card"));
      cards.forEach(function (card) {
        var fresh = freshCards.find(function (c) { return c.dataset.deviceId === card.dataset.deviceId; });
        if (fresh) card.innerHTML = fresh.innerHTML;
      });
      if (cards.length !== freshCards.length || cards.some(function (c) { return !freshCards.some(function (n) { return n.dataset.deviceId === c.dataset.deviceId; }); })) {
        error.hidden = false;
        error.textContent = "Device roster changed. Reload this page to update the inventory.";
      }
    }).catch(function () {
      var error = document.getElementById("refresh-error");
      if (error) { error.hidden = false; error.textContent = "Refresh unavailable. Showing last known data; retrying every 30 seconds. Reload if your sign-in has expired."; }
    });
  }

  function loadClients(deviceId) {
    var tbody = document.querySelector("#clients-table tbody");
    if (!tbody) return;
    var button = document.querySelector("[data-action=refresh_clients]");
    if (button) { button.disabled = true; button.textContent = "Refreshing…"; }
    return fetchJSON("/api/device/" + deviceId + "/clients").then(function (r) {
      if (button) { button.disabled = false; button.textContent = "Refresh"; }
      if (!r.ok) {
        tbody.innerHTML = '<tr><td colspan="5" class="muted">' + esc(r.body.error || "failed to load clients") + "</td></tr>";
        return;
      }
      var c = r.body.clients || [];
      loadSummary();
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
      if (!/^\d+$/.test(start) || !/^\d+$/.test(dur) || Number(start) > 1439 || Number(dur) < 1 || Number(dur) > 1440) {
        showOutcome({ ok: false, body: { error: "Enter whole minutes: start 0–1439 UTC, duration 1–1440." } });
        return;
      }
      if (!confirm("Enable power save starting " + start + " minutes past midnight UTC for " + dur + " minutes?")) return;
      sendAction(btn, id, {
        action: "power_save", enable: true,
        startMinutes: Number(start), durationMinutes: Number(dur)
      });
      return;
    }
    if (action === "disable_power_save") {
      if (confirm("Disable scheduled power save?")) sendAction(btn, id, { action: "power_save", enable: false });
      return;
    }

    var msg = CONFIRM_TEXT[action];
    if (!msg) return;
    if (!confirm(msg)) return;
    ev.preventDefault();
    sendAction(btn, id, { action: action });
  });

  function sendAction(btn, id, body) {
    var buttons = Array.from(document.querySelectorAll(".action-card button"));
    var label = btn.textContent;
    buttons.forEach(function (b) { b.disabled = true; });
    btn.textContent = "Sending…";
    btn.setAttribute("aria-busy", "true");
    var box = document.getElementById("action-outcome");
    if (box) { box.hidden = false; box.className = "outcome"; box.textContent = "Sending command. Please wait…"; }
    postJSON("/api/device/" + id + "/action", body).then(function (r) {
      showOutcome(r);
      buttons.forEach(function (b) { b.disabled = false; });
      btn.textContent = label;
      btn.removeAttribute("aria-busy");
      if (document.activeElement === document.body) btn.focus();
    });
  }
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
    if (!document.querySelector(".error-card")) {
      setInterval(loadSummary, 30000);
    }
  });
})();
