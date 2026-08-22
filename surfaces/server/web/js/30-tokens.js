/* 30-tokens.js — extracted from app.js (lines 433-488) */
// LIVE TOKEN TELEMETRY
// ════════════════════════════════════════════════════════════════════════
function handleTokenUsage(data) {
  const stats = data.content || data;
  // Update topbar meters with flash animation
  flashMeter("meter-tokens", fmtNum(stats.cumulative_tokens || 0));
  flashMeter("meter-cost", fmtCost(stats.cumulative_cost || 0));
  flashMeter("meter-reqs", fmtNum(stats.cumulative_requests || 0));

  // Track sparkline data
  sparklineData.tokens.push(stats.total_tokens || 0);
  sparklineData.cost.push(stats.cost || 0);
  sparklineData.reqs.push(1);
  if (sparklineData.tokens.length > SPARK_MAX) {
    sparklineData.tokens.shift();
    sparklineData.cost.shift();
    sparklineData.reqs.shift();
  }

  // Surface as a compact event
  addEvent({
    type: "token_usage",
    agent: stats.provider || "",
    status: "usage",
    content: `+${fmtNum(stats.total_tokens)} tokens (${fmtCost(stats.cost)}) — ${stats.model}`,
    timestamp: new Date().toISOString(),
  });

  if (activeTab === "monitoring") debouncedMetricsRefresh();
}

function flashMeter(id, value) {
  const el = $("#" + id);
  if (!el) return;
  el.textContent = value;
  el.classList.add("flash");
  setTimeout(() => el.classList.remove("flash"), 400);
}

let _mTimer = null;
function debouncedMetricsRefresh() {
  if (metricsRefreshPending) return;
  metricsRefreshPending = true;
  clearTimeout(_mTimer);
  _mTimer = setTimeout(() => { metricsRefreshPending = false; loadMetrics(); }, 1200);
}

function startMetricsPolling() {
  stopMetricsPolling();
  // (P4) Skip the fetch while the document is hidden — the monitoring tab
  // is not visible, so the chart redraw would be wasted work.
  metricsPollTimer = setInterval(() => { if (activeTab === "monitoring" && !document.hidden) loadMetrics(); }, 5000);
}
function stopMetricsPolling() {
  if (metricsPollTimer) { clearInterval(metricsPollTimer); metricsPollTimer = null; }
}

// ════════════════════════════════════════════════════════════════════════
