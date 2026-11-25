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

  /* ---------- SVG line chart ---------- */
  function drawChart(svg, series, opts) {
    var ns = "http://www.w3.org/2000/svg";
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    if (!series || series.length < 1) return;

    var W = 400, H = 140, padL = 44, padB = 18, padT = 8, padR = 8;
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    svg.setAttribute("preserveAspectRatio", "none");

    var now = Math.floor(Date.now() / 1000);
    var tMin = opts.tMin != null ? opts.tMin : series[0].t;
    var tMax = Math.max(now, series[series.length - 1].t);
    if (tMax === tMin) tMax = tMin + 1;

    var vMin = Infinity, vMax = -Infinity;
    series.forEach(function (p) { vMin = Math.min(vMin, p.v); vMax = Math.max(vMax, p.v); });
    if (vMin === vMax) { vMin -= 1; vMax += 1; }
    var span = vMax - vMin;
    vMin -= span * 0.06; vMax += span * 0.06;

    function x(t) { return padL + (t - tMin) / (tMax - tMin) * (W - padL - padR); }
    function y(v) { return padT + (vMax - v) / (vMax - vMin) * (H - padT - padB); }

    /* gridlines + y labels */
    for (var g = 0; g <= 3; g++) {
      var v = vMin + (vMax - vMin) * g / 3;
      var gy = y(v);
      var line = document.createElementNS(ns, "line");
      line.setAttribute("x1", padL); line.setAttribute("x2", W - padR);
      line.setAttribute("y1", gy); line.setAttribute("y2", gy);
      line.setAttribute("stroke", "#2a3442"); line.setAttribute("stroke-width", "1");
      svg.appendChild(line);
      var lbl = document.createElementNS(ns, "text");
      lbl.setAttribute("x", padL - 4); lbl.setAttribute("y", gy + 3);
      lbl.setAttribute("text-anchor", "end");
      lbl.textContent = opts.fmt(v);
      svg.appendChild(lbl);
    }
    /* x labels: tMin and tMax */
    [[x(tMin), fmtAgo(now - tMin)], [x(tMax), "now"]].forEach(function (p) {
      var xl = document.createElementNS(ns, "text");
      xl.setAttribute("x", p[0]); xl.setAttribute("y", H - 4);
      xl.setAttribute("text-anchor", p[1] === "now" ? "end" : "start");
      xl.textContent = p[1];
      svg.appendChild(xl);
    });

    /* area + line path */
    var d = "";
    series.forEach(function (p, i) {
      d += (i === 0 ? "M" : "L") + x(p.t).toFixed(1) + " " + y(p.v).toFixed(1);
    });
    var area = document.createElementNS(ns, "path");
    area.setAttribute("d", d + " L" + x(series[series.length - 1].t).toFixed(1) + " " + (H - padB) +
      " L" + x(series[0].t).toFixed(1) + " " + (H - padB) + " Z");
    area.setAttribute("fill", opts.color); area.setAttribute("opacity", "0.12");
    svg.appendChild(area);
    var path = document.createElementNS(ns, "path");
    path.setAttribute("d", d);
    path.setAttribute("fill", "none");
    path.setAttribute("stroke", opts.color);
    path.setAttribute("stroke-width", "1.5");
    path.setAttribute("vector-effect", "non-scaling-stroke");
    svg.appendChild(path);
  }

  /* summary formatters receive plain numbers (the plotted values) */
  function fmtMs(v) { return v.toFixed(0) + " ms"; }
  function fmtPct(v) { return v.toFixed(1) + " %"; }

  var CHARTS = {
    ping:     { title: "Ping latency (ms)", color: "#6fb6ff", fmt: function (v) { return v.toFixed(0); },
                pick: function (s) { return { t: s.ts, v: s.pingMs }; }, summary: function (a) { return avgSum(a, fmtMs); } },
    /* dropRate is a 0..1 fraction; plot it as a percentage */
    drop:     { title: "Ping drop rate (%)", color: "#e05252", fmt: function (v) { return v.toFixed(1); },
                pick: function (s) { return { t: s.ts, v: s.dropRate * 100 }; }, summary: function (a) { return avgSum(a, fmtPct); } },
    downlink: { title: "Downlink throughput", color: "#35b57a", fmt: fmtBytes,
                pick: function (s) { return { t: s.ts, v: s.downBps }; }, summary: function (a) { return avgSum(a, fmtBytes); } },
    uplink:   { title: "Uplink throughput", color: "#f5c211", fmt: fmtBytes,
                pick: function (s) { return { t: s.ts, v: s.upBps }; }, summary: function (a) { return avgSum(a, fmtBytes); } },
    obstruction: { title: "Obstruction", color: "#e05252", fmt: function (v) { return v.toFixed(0); },
                pick: function (s) { return { t: s.ts, v: s.obstructed ? 1 : 0 }; },
                summary: function (a) { return fractionSummary(a, "of samples obstructed"); } },
    scheduled: { title: "Scheduled (boresight) windows", color: "#f5a33d", fmt: function (v) { return v.toFixed(0); },
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

  function loadHistory(deviceId, win) {
    var names = Object.keys(CHARTS);
    fetchJSON("/api/device/" + deviceId + "/history?window=" + encodeURIComponent(win))
      .then(function (r) {
        if (!r.ok) return;
        var samples = r.body.samples || [];
        names.forEach(function (key) {
          var svg = document.querySelector('svg[data-chart="' + key + '"]');
          var sum = document.querySelector('[data-chart-summary="' + key + '"]');
          var series = samples.map(CHARTS[key].pick).filter(function (p) { return p.v != null && isFinite(p.v); });
          if (svg) drawChart(svg, series, { color: CHARTS[key].color, fmt: CHARTS[key].fmt, tMin: r.body.tMin });
          if (sum) sum.textContent = CHARTS[key].summary(series.map(function (p) { return p.v; })).replace("{v}", "");
        });
      });
  }

  function loadSummary() {
    fetchJSON("/api/summary").then(function (r) {
      if (!r.ok) return;
      var info = document.querySelector("[data-poll-info]");
      if (info) {
        var d = r.body;
        info.textContent = (d.sessionDead ? "⚠ session invalid · " : "") +
          (d.devices || []).length + " device(s) · refreshed " + fmtAgo((Date.now() / 1000) - d.updatedAt);
      }
      (r.body.devices || []).forEach(function (dev) {
        var root = document.querySelector('[data-device-id="' + dev.id + '"]');
        if (!root) return;
        if (dev.onlineLabel != null) {
          var pill = root.querySelector('[data-field="online"]');
          if (pill) { pill.textContent = dev.onlineLabel; pill.className = "pill " + dev.onlineClass; }
        }
        ["state", "snr", "obstruction", "versions", "alerts", "updated"].forEach(function (f) {
          if (dev[f] == null) return;
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
      var start = prompt("Power save start minutes after midnight UTC (0 = 00:00):", "0");
      if (start === null) return;
      var dur = prompt("Duration in minutes:", "480");
      if (dur === null) return;
      var enable = confirm("Enable scheduled power save (OK) or disable it (Cancel)?\nOK = enable with start " + start + " for " + dur + " minutes\nCancel = disable power save");
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
    var charts = document.querySelectorAll("svg[data-chart]");
    var deviceId = charts.length ? charts[0].getAttribute("data-device-id") : null;
    if (deviceId) {
      loadHistory(deviceId, currentWindow());
      document.querySelectorAll("button[data-window]").forEach(function (b) {
        b.addEventListener("click", function () {
          document.querySelectorAll("button[data-window]").forEach(function (x) { x.classList.remove("btn-selected"); });
          b.classList.add("btn-selected");
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
