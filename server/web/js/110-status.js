/* 110-status.js — extracted from app.js (lines 2305-2484) */
// LEARNING ENGINE
// ════════════════════════════════════════════════════════════════════════
async function loadLearning() {
  const grid = $("#learn-grid");
  if (!grid) return;
  grid.innerHTML = "<div class='mem-empty'>Loading...</div>";
  try {
    const res = await fetch(API + "/api/learning/stats");
    const data = await res.json();
    const stats = $("#learn-stats"); if (stats) stats.textContent = `${data.stats?.total_tasks || 0} Tasks / ${data.stats?.success_rate || 0}% Success`;
    grid.innerHTML = "";
    if (!data.strategies || !data.strategies.length) {
      grid.innerHTML = "<div class='mem-empty'>No strategies learned yet. Run more tasks.</div>";
      return;
    }
    data.strategies.forEach(s => {
      grid.innerHTML += `
        <div class="s-tile">
          <div class="s-tile-label">${esc(s.task_type)} strategy</div>
          <div style="font-size:14px; font-weight:600; margin-bottom:8px">${esc(s.name)}</div>
          <div style="font-size:12px; color:var(--text-dim); margin-bottom:12px">${esc(s.description)}</div>
          <div style="display:flex; justify-content:space-between; margin-bottom:8px">
             <span style="font-size:11px; color:var(--green)">Success: ${s.success_count}</span>
             <span style="font-size:11px; color:var(--red)">Fail: ${s.fail_count}</span>
          </div>
          <div class="s-layers" style="margin-top:8px">
            ${(s.preferred_tools || []).map(t => `<span class="s-layer" style="font-size:10px; padding:2px 6px;">${esc(t)}</span>`).join("")}
          </div>
        </div>`;
    });
  } catch(e) {
    grid.innerHTML = `<div class="mem-empty">Failed: ${esc(e.message)}</div>`;
  }
}

// ════════════════════════════════════════════════════════════════════════
// AUDIT LOG
// ════════════════════════════════════════════════════════════════════════
async function loadAudit() {
  const list = $("#audit-list");
  if (!list) return;
  list.innerHTML = "<div class='mem-empty'>Loading...</div>";
  try {
    const res = await fetch(API + "/api/audit/recent");
    const data = await res.json();
    const countRes = await fetch(API + "/api/audit");
    if(countRes.ok) {
       const cd = await countRes.json();
       const stats = $("#audit-stats"); if (stats) stats.textContent = `${cd.count || 0} Logs / ${cd.summary?.denied || 0} Denied`;
    }
    list.innerHTML = `
      <div class="audit-item audit-header">
        <div>Timestamp</div><div>Risk</div><div>Agent</div><div>Action / Details</div><div>Result</div>
      </div>`;
    if (!data.entries || !data.entries.length) {
      list.innerHTML += "<div class='mem-empty'>No audit logs.</div>";
      return;
    }
    data.entries.forEach(e => {
      const riskClass = `risk-${(e.risk_level||"low").toLowerCase()}`;
      const resClass = e.approved ? 'risk-low' : 'risk-high';
      list.innerHTML += `
        <div class="audit-item">
          <div style="color:var(--text-dim)">${fmtTime(e.timestamp)}</div>
          <div class="${riskClass}">${esc(e.risk_level)}</div>
          <div style="color:var(--accent-3); font-weight:600">${esc(e.agent)}</div>
          <div><strong style="color:var(--text-bright)">${esc(e.action)}</strong>: ${esc(e.tool || '-')}</div>
          <div class="${resClass}">${e.approved ? 'APPROVED' : 'DENIED'}</div>
        </div>`;
    });
  } catch(e) {
    list.innerHTML = `<div class="mem-empty">Failed: ${esc(e.message)}</div>`;
  }
}

// ════════════════════════════════════════════════════════════════════════
// STATUS
// ════════════════════════════════════════════════════════════════════════
async function loadStatus() {
  const c = $("#status-content");
  if (!c) return;
  c.innerHTML = '<div class="mem-empty">Loading…</div>';
  try {
    const [resStatus, resCap] = await Promise.all([
      fetch(API + "/api/status"),
      fetch(API + "/api/capability")
    ]);
    const d = await resStatus.json();
    const cap = await resCap.json();
    const tools = d.tools || [];
    const m = d.metrics || {};
    c.innerHTML = `
      ${m.total_tokens !== undefined ? `
      <div class="s-section">
        <h3>Live Usage</h3>
        <div class="s-grid">
          ${sTile("Total Tokens", fmtNum(m.total_tokens || 0))}
          ${sTile("Estimated Cost", fmtCost(m.total_cost || 0))}
          ${sTile("LLM Calls", m.total_requests || 0)}
          ${sTile("Questions", m.total_turns || 0)}
          ${sTile("Avg Latency", Math.round(m.avg_latency_ms || 0) + "ms")}
        </div>
      </div>` : ""}
      <div class="s-section">
        <h3>Model &amp; Routing</h3>
        <div class="s-grid">
          ${sTile("Model", d.model)}
          ${sTile("Provider", d.provider || "-")}
          ${sTile("Base URL", d.base_url)}
          ${sTile("Routing Mode", d.routing_mode)}
          ${sTile("Safety Level", d.safety_level)}
          ${sTile("Max Turns", d.max_turns)}
        </div>
      </div>
      <div class="s-section">
        <h3>Memory</h3>
        <div class="s-grid">
          ${sTile("Types", (d.memory_types || []).join(", "))}
          ${sTile("Skills", d.skill_count)}
          ${sTile("Episodes", d.episode_count)}
        </div>
      </div>
      <div class="s-section">
        <h3>Tool Runtime (${d.tool_count} tools)</h3>
        <div class="s-grid">
          ${tools.map((t) => sTile(t.name, t.category)).join("")}
        </div>
      </div>
      <div class="s-section">
        <h3>Architecture Layers</h3>
        <div class="s-layers">
          ${(d.layers || []).map((l) => `<span class="s-layer">${esc(l)}</span>`).join("")}
        </div>
      </div>`;
    updateBadges(d);
    updateMetersFromSnap(m);
    if (cap.hardware) {
      updateHardwareUI(cap.hardware, cap.tier);
    } else if (d.hardware) {
      updateHardwareUI(d.hardware, "Unknown");
    }
  } catch (err) {
    c.innerHTML = `<div class="mem-empty">Failed: ${esc(err.message)}</div>`;
  }
}

function updateHardwareUI(hw, tier) {
  const setHtml = (id, val) => { const el = $(id); if (el) el.innerHTML = val; };
  if (tier) setHtml("#hw-execution-tier", `Execution Tier: ${tier}`);
  setHtml("#hw-cpu", `${hw.cpu_usage_percent.toFixed(1)}%`);
  setHtml("#hw-ram", `${Math.round(hw.ram_used_mb || 0)} MB <span style="font-size:10px;color:var(--text-dim)">/ ${Math.round(hw.ram_total_mb || 0)} MB</span>`);
  setHtml("#hw-threads", hw.go_routines);
  setHtml("#hw-heap", `${Math.round(hw.go_heap_alloc_mb || 0)} MB`);
  let meta = `${hw.os} ${hw.arch} / ${hw.num_cpu} cores`;
  if (hw.provider_mode === "local" && hw.vram_used_mb > 0) {
    meta += ` <span style="color:var(--purple); margin-left: 8px;">GPU VRAM: ${Math.round(hw.vram_used_mb)} MB</span>`;
    setHtml("#hw-vram", `${Math.round(hw.vram_used_mb)} MB`);
  } else if (hw.provider_mode === "local") {
    setHtml("#hw-vram", "0 MB");
  } else {
    setHtml("#hw-vram", "N/A (Cloud)");
  }
  setHtml("#hw-os-arch", meta);
}

function sTile(label, value) {
  return `<div class="s-tile"><div class="s-tile-label">${esc(label)}</div><div class="s-tile-value">${esc(String(value))}</div></div>`;
}

function updateBadges(d) {
  const b = $("#badges");
  if (!b) return;
  b.innerHTML = "";
  // Model name lives in the model-switcher dropdown; showing it here too made
  // the header overflow (model appeared 3×). Keep only the routing + safety
  // glance pills so the header-right cluster fits within the viewport.
  if (d.routing_mode) b.innerHTML += `<span class="badge blue">${esc(d.routing_mode)}</span>`;
  if (d.safety_level) b.innerHTML += `<span class="badge yellow">${esc(d.safety_level)}</span>`;
}

// ════════════════════════════════════════════════════════════════════════
