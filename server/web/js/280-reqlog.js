// 280-reqlog.js — the individual model calls behind the telemetry totals.
//
// metrics.Default has always kept a rolling record per request: model,
// provider, prompt and completion tokens, how many of those were served from
// the provider's prefix cache, cost, latency, whether it streamed and whether
// it succeeded. /api/metrics/requests has always served it. Nothing displayed
// it, so the GUI could tell you that you had spent $0.40 across 60 requests
// and not which request, which model, or why.
//
// The totals answer "how much". This answers "on what", which is the question
// you actually act on — one model quietly costing ten times another, a retry
// storm, a cache that stopped being hit.

(function () {
  const $ = (id) => document.getElementById(id);

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  const fmtCost = (c) => (c || 0) < 0.01 ? '$' + (c || 0).toFixed(5) : '$' + (c || 0).toFixed(4);
  const fmtNum = (n) => (n || 0).toLocaleString();

  function fmtTime(ts) {
    if (!ts) return '';
    const d = new Date(ts);
    return isNaN(d) ? '' : d.toLocaleTimeString();
  }

  // Latency earns colour because it is the column people scan for outliers.
  function latClass(ms) {
    if (ms >= 20000) return 'rl-bad';
    if (ms >= 8000) return 'rl-warn';
    return '';
  }

  function render(requests) {
    const box = $('req-log');
    if (!box) return;

    if (!requests.length) {
      box.innerHTML = '<div class="mem-empty" style="padding:16px;">No model calls recorded yet.</div>';
      const st = $('req-log-stats');
      if (st) st.textContent = '—';
      return;
    }

    // Newest first: the call you care about is nearly always the last one.
    const rows = requests.slice().reverse();
    const totalCost = rows.reduce((s, r) => s + (r.cost || 0), 0);
    const cached = rows.reduce((s, r) => s + (r.cached_tokens || 0), 0);
    const failed = rows.filter((r) => r.success === false).length;

    const st = $('req-log-stats');
    if (st) {
      const bits = [`${rows.length} calls`, fmtCost(totalCost)];
      if (cached > 0) bits.push(`${fmtNum(cached)} cached`);
      if (failed > 0) bits.push(`${failed} failed`);
      st.textContent = bits.join(' · ');
    }

    box.innerHTML = `<table class="reqlog">
      <thead><tr>
        <th>Time</th><th>Model</th><th class="num">In</th><th class="num">Cached</th>
        <th class="num">Out</th><th class="num">Cost</th><th class="num">Latency</th><th></th>
      </tr></thead>
      <tbody>${rows.map((r) => {
        const ms = r.latency_ms || 0;
        return `<tr class="${r.success === false ? 'rl-failed' : ''}">
          <td class="rl-time">${esc(fmtTime(r.timestamp))}</td>
          <td class="rl-model" title="${esc(r.provider || '')}">${esc(r.model || '—')}</td>
          <td class="num">${fmtNum(r.prompt_tokens)}</td>
          <td class="num ${r.cached_tokens ? 'rl-cached' : 'rl-zero'}">${r.cached_tokens ? fmtNum(r.cached_tokens) : '—'}</td>
          <td class="num">${fmtNum(r.completion_tokens)}</td>
          <td class="num">${fmtCost(r.cost)}</td>
          <td class="num ${latClass(ms)}">${ms ? fmtNum(ms) + 'ms' : '—'}</td>
          <td class="rl-flags">${r.stream ? '<span title="streamed">⇢</span>' : ''}${
            r.success === false ? '<span title="failed">✕</span>' : ''}</td>
        </tr>`;
      }).join('')}</tbody></table>`;
  }

  async function load() {
    try {
      const res = await fetch('/api/metrics/requests');
      const data = await res.json();
      render(data.requests || []);
    } catch (e) {
      const box = $('req-log');
      if (box) box.innerHTML = '<div class="mem-empty" style="padding:16px;">Could not read the request log.</div>';
    }
  }

  function init() {
    if (!$('req-log')) return false;
    const btn = $('req-log-refresh');
    if (btn) btn.addEventListener('click', load);
    load();
    // Refresh only while the tab is actually being looked at. A hidden panel
    // polling in the background is exactly the kind of cost this codebase has
    // been paying down, not adding to.
    setInterval(() => {
      const panel = document.getElementById('tab-telemetry');
      if (panel && panel.classList.contains('active') && !document.hidden) load();
    }, 5000);
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
