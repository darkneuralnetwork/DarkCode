/* 120-metrics.js — extracted from app.js (lines 2485-2697) */
// MONITORING DASHBOARD
// ════════════════════════════════════════════════════════════════════════
async function loadMetrics() {
  if (!$("#kpi-total-tokens")) return;
  try {
    const res = await fetch(API + "/api/metrics/tokens");
    if (!res.ok) return;
    const snap = await res.json();
    renderMetrics(snap);
  } catch (err) {
    console.error("metrics load failed", err);
  }
}

function renderMetrics(snap) {
  updateMetersFromSnap(snap);

  const safeSet = (sel, val) => { const e = $(sel); if (e) e.textContent = val; };
  animateValue("kpi-total-tokens", snap.total_tokens || 0, fmtNum);
  safeSet("#kpi-prompt", fmtNum(snap.total_prompt_tokens || 0));
  safeSet("#kpi-completion", fmtNum(snap.total_completion_tokens || 0));
  safeSet("#kpi-cost", fmtCost(snap.total_cost || 0));
  // Prompt-cache accounting: show cached prompt tokens and the estimated USD
  // saved by billing them at the cheaper cached rate. Appended to the cost
  // KPI's sublabel when present so it needs no new DOM element.
  const cachedTok = snap.total_cached_tokens || 0;
  const saved = snap.cache_savings || 0;
  if (cachedTok > 0) {
    safeSet("#kpi-cache", `${fmtNum(cachedTok)} cached · ${fmtCost(saved)} saved`);
  } else {
    safeSet("#kpi-cache", "");
  }
  safeSet("#kpi-since", fmtTimeShort(snap.since));
  animateValue("kpi-requests", snap.total_requests || 0, fmtNum);

  // Questions (user turns) vs LLM calls: one question fans out into several
  // calls, so surface both plus the ratio so the call count isn't mistaken for
  // "one call per question".
  const turns = snap.total_turns || 0;
  animateValue("kpi-turns", turns, fmtNum);
  const callsPerTurn = turns > 0 ? (snap.total_requests || 0) / turns : 0;
  safeSet("#kpi-calls-per-turn", callsPerTurn ? callsPerTurn.toFixed(1) : "0");

  const errs = snap.total_errors || 0;
  const succRate = snap.total_requests > 0 ? Math.round(((snap.total_requests - errs) / snap.total_requests) * 100) : 100;
  safeSet("#kpi-errors", errs + " errors");
  safeSet("#kpi-success-rate", succRate + "% success");
  safeSet("#kpi-latency", fmtDur(Math.round(snap.avg_latency_ms || 0)));

  let rpm = 0;
  if (snap.series && snap.series.length) rpm = snap.series[snap.series.length-1].requests || 0;
  safeSet("#kpi-rpm", rpm + " req/min");

  renderCharts(snap);
  renderRecentRequests(snap.recent || []);
}

function animateValue(id, target, formatter) {
  const el = $("#" + id);
  if (!el) return;
  const current = parseInt(el.dataset.val || "0", 10);
  if (current === target) { el.textContent = formatter(target); return; }
  const start = current;
  const diff = target - start;
  const duration = 500;
  const startTime = performance.now();
  function step(now) {
    const t = Math.min((now - startTime) / duration, 1);
    const eased = 1 - Math.pow(1 - t, 3);
    const val = Math.round(start + diff * eased);
    el.textContent = formatter(val);
    if (t < 1) requestAnimationFrame(step);
    else el.dataset.val = String(target);
  }
  requestAnimationFrame(step);
}

function updateMetersFromSnap(snap) {
  const tok = $("#meter-tokens"); if (tok) tok.textContent = fmtNum(snap.total_tokens || snap.cumulative_tokens || 0);
  const cost = $("#meter-cost"); if (cost) cost.textContent = fmtCost(snap.total_cost || snap.cumulative_cost || 0);
  const reqs = $("#meter-reqs"); if (reqs) reqs.textContent = fmtNum(snap.total_requests || snap.cumulative_requests || 0);
}

function applyChartTheme() {
  if (typeof Chart === "undefined") return;
  Chart.defaults.color = C.dim;
  Chart.defaults.borderColor = C.grid;
  Chart.defaults.font.family = "'JetBrains Mono', monospace";
  Chart.defaults.font.size = 11;
}

function renderCharts(snap) {
  if (typeof Chart === "undefined") return;
  applyChartTheme();
  const series = snap.series || [];
  const labels = series.map(b => fmtTimeShort(b.bucket));

  // 1. Tokens over time (stacked area)
  const tokCtx = $("#chart-tokens-time");
  if (tokCtx) {
    const data = {
      labels,
      datasets: [
        { label: "Prompt", data: series.map(b => b.prompt_tokens), borderColor: C.blue, backgroundColor: "rgba(41,182,246,0.2)", fill: true, tension: 0.4, pointRadius: 0, borderWidth: 2 },
        { label: "Completion", data: series.map(b => b.completion_tokens), borderColor: C.orange, backgroundColor: "rgba(255,107,0,0.2)", fill: true, tension: 0.4, pointRadius: 0, borderWidth: 2 },
      ]
    };
    charts.tokens = upsertLine(charts.tokens, tokCtx, data, true);
  }

  // 2. Tokens by model (doughnut)
  const mtCtx = $("#chart-model-tokens");
  if (mtCtx) {
    const pm = snap.per_model || [];
    const modelData = {
      labels: pm.map(m => m.model),
      datasets: [{
        data: pm.map(m => m.total_tokens),
        backgroundColor: pm.map((_, i) => MODEL_COLORS[i % MODEL_COLORS.length]),
        borderColor: "#0a0a0d",
        borderWidth: 3,
        hoverOffset: 8,
      }]
    };
    if (pm.length === 0) { modelData.labels = ["No data"]; modelData.datasets[0].data = [1]; modelData.datasets[0].backgroundColor = ["#232330"]; }
    charts.modelTokens = upsertDoughnut(charts.modelTokens, mtCtx, modelData);
  }

  // 3. Cost over time (cumulative area)
  const costCtx = $("#chart-cost-time");
  if (costCtx) {
    let cum = 0;
    const cumData = series.map(b => { cum += b.cost; return cum; });
    const data = {
      labels,
      datasets: [{ label: "Cost (USD)", data: cumData, borderColor: C.green, backgroundColor: "rgba(0,230,118,0.15)", fill: true, tension: 0.4, pointRadius: 0, borderWidth: 2 }]
    };
    charts.cost = upsertLine(charts.cost, costCtx, data, false);
  }

  // 4. Requests per minute (bar)
  const rpmCtx = $("#chart-rpm");
  if (rpmCtx) {
    const data = {
      labels,
      datasets: [{ label: "Requests", data: series.map(b => b.requests), backgroundColor: C.amber, borderRadius: 4, barThickness: "flex" }]
    };
    charts.rpm = upsertBar(charts.rpm, rpmCtx, data);
  }

  // 5. Latency trend
  const latCtx = $("#chart-latency");
  if (latCtx) {
    const recent = (snap.recent || []).slice(-30).reverse();
    const data = {
      labels: recent.map(r => fmtTime(r.timestamp)),
      datasets: [{ label: "Latency (ms)", data: recent.map(r => r.latency_ms), borderColor: C.yellow, backgroundColor: "rgba(255,213,79,0.15)", fill: true, tension: 0.4, pointRadius: 2, borderWidth: 2 }]
    };
    charts.latency = upsertLine(charts.latency, latCtx, data, false);
  }
}

function upsertLine(existing, ctx, data, stacked) {
  if (existing) { existing.data = data; existing.update("none"); return existing; }
  return new Chart(ctx, {
    type: "line",
    data,
    options: {
      responsive: true, maintainAspectRatio: false,
      interaction: { intersect: false, mode: "index" },
      plugins: {
        legend: { labels: { boxWidth: 10, boxHeight: 10, usePointStyle: true } },
        tooltip: { backgroundColor: "#16161e", borderColor: "#2e2e3d", borderWidth: 1, padding: 10, cornerRadius: 8, titleColor: "#ff9100", bodyColor: "#e8e8f0" }
      },
      scales: {
        x: { stacked: stacked, grid: { color: C.grid, display: false }, ticks: { maxRotation: 0 } },
        y: { stacked: stacked, grid: { color: C.grid }, beginAtZero: true }
      }
    }
  });
}

function upsertBar(existing, ctx, data) {
  if (existing) { existing.data = data; existing.update("none"); return existing; }
  return new Chart(ctx, {
    type: "bar",
    data,
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: { legend: { display: false }, tooltip: { backgroundColor: "#16161e", borderColor: "#2e2e3d", borderWidth: 1, padding: 10, cornerRadius: 8 } },
      scales: { x: { grid: { color: C.grid, display: false } }, y: { grid: { color: C.grid }, beginAtZero: true } }
    }
  });
}

function upsertDoughnut(existing, ctx, data) {
  if (existing) { existing.data = data; existing.update("none"); return existing; }
  return new Chart(ctx, {
    type: "doughnut",
    data,
    options: {
      responsive: true, maintainAspectRatio: false,
      cutout: "62%",
      plugins: { legend: { position: "bottom", labels: { boxWidth: 10, boxHeight: 10, usePointStyle: true, padding: 12 } }, tooltip: { backgroundColor: "#16161e", borderColor: "#2e2e3d", borderWidth: 1, padding: 10, cornerRadius: 8 } }
    }
  });
}

function renderRecentRequests(recent) {
  const tbody = $("#req-tbody");
  if (!tbody) return;
  if (!recent.length) {
    tbody.innerHTML = `<tr><td colspan="10" class="mem-empty">No requests recorded yet. Send a chat to generate telemetry.</td></tr>`;
    return;
  }
  tbody.innerHTML = recent.slice().reverse().slice(0, 100).map(r => `
    <tr>
      <td>${fmtTime(r.timestamp)}</td>
      <td><span class="req-prov">${esc(r.provider || "-")}</span></td>
      <td>${esc(r.model || "-")}</td>
      <td>${fmtNum(r.prompt_tokens)}</td>
      <td>${fmtNum(r.completion_tokens)}</td>
      <td>${fmtNum(r.total_tokens)}</td>
      <td>${fmtCost(r.cost)}</td>
      <td>${fmtDur(r.latency_ms)}</td>
      <td>${r.stream ? "stream" : "sync"}</td>
      <td class="${r.success ? "req-status-ok" : "req-status-err"}">${r.success ? "✓ OK" : "✗ ERR"}</td>
    </tr>`).join("");
}

// ════════════════════════════════════════════════════════════════════════
