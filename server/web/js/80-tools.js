/* 80-tools.js — extracted from app.js (lines 1587-1750) */
// TOOLS
// ════════════════════════════════════════════════════════════════════════
async function loadTools() {
  const grid = $("#tools-grid");
  if (!grid) return;
  grid.innerHTML = '<div class="mem-empty">Loading…</div>';
  // Load sources in parallel with the tool list.
  loadToolSources();
  try {
    const res = await fetch(API + "/api/tools");
    const data = await res.json();
    const tools = data.tools || [];
    const tc = $("#tools-count"); if (tc) tc.textContent = `${tools.length} tools`;
    grid.innerHTML = "";
    if (!tools.length) { grid.innerHTML = '<div class="mem-empty">No tools registered.</div>'; return; }
    tools.forEach((t) => {
      const source = t.source || "builtin";
      const card = document.createElement("div");
      card.className = "tool-list-item";
      card.innerHTML = `
        <span class="tool-list-name">${esc(t.name)}</span>
        <span class="tool-list-desc" title="${esc(t.description || "")}">${esc(t.description || "")}</span>
        <div class="tool-list-meta">
          <span class="tool-cat" style="margin-bottom:0;">${esc(t.category || "general")}</span>
          <span class="req-prov">${esc(source)}</span>
        </div>`;
      grid.appendChild(card);
    });
  } catch (err) {
    grid.innerHTML = `<div class="mem-empty">Failed to load: ${esc(err.message)}</div>`;
  }
}

// ════════════════════════════════════════════════════════════════════════
// TOOL SOURCES — connect/disconnect MCP servers & in-house ITF tools at runtime
// ════════════════════════════════════════════════════════════════════════
async function loadToolSources() {
  const list = $("#tools-sources-list");
  const stats = $("#tools-src-stats");
  if (!list) return;
  try {
    const res = await fetch(API + "/api/tools/sources");
    if (!res.ok) { list.innerHTML = `<div class="mem-empty">Failed (HTTP ${res.status})</div>`; return; }
    const data = await res.json();
    const srcs = data.sources || [];
    const connected = srcs.filter((s) => s.status === "connected").length;
    if (stats) stats.textContent = `${connected}/${srcs.length} sources connected`;
    if (!srcs.length) {
      list.innerHTML = '<div class="mem-empty">No tool sources yet. Add one below to connect an MCP server or load in-house tools.</div>';
      return;
    }
    list.innerHTML = srcs.map(renderToolSourceCard).join("");
    list.querySelectorAll("button[data-act]").forEach((btn) => {
      btn.addEventListener("click", () => toolSourceAction(btn.dataset.act, btn.dataset.id));
    });
  } catch (err) {
    list.innerHTML = `<div class="mem-empty">Failed: ${esc(err.message)}</div>`;
  }
}

function renderToolSourceCard(s) {
  const statusCls = {
    connected: "risk-low",
    connecting: "risk-medium",
    error: "risk-high",
    disconnected: "",
  }[s.status] || "";
  const dot = { connected: "●", connecting: "●", error: "✗", disconnected: "○" }[s.status] || "○";
  let detail = "";
  if (s.config.type === "mcp_stdio") detail = `${s.config.command || ""} ${(s.config.args || []).join(" ")}`.trim();
  else if (s.config.type === "mcp_http") detail = s.config.url || "";
  else if (s.config.type === "internal") detail = s.config.path || "";
  const tools = (s.tools || []).length;
  const isConnected = s.status === "connected";
  return `
    <div class="model-card">
      <div class="mc-head">
        <div class="mc-name">${esc(s.config.name)}
          <span class="mem-tag ${statusCls}">${dot} ${esc(s.status)}</span>
          <span class="mc-tier">${esc(s.config.type)}</span>
        </div>
        <div class="mc-actions">
          ${isConnected
            ? `<button class="btn-glow btn-xs" data-act="disconnect" data-id="${esc(s.config.id)}">Disconnect</button>`
            : `<button class="btn-glow btn-xs" data-act="connect" data-id="${esc(s.config.id)}">Connect</button>`}
          <button class="btn-glow btn-xs danger" data-act="remove" data-id="${esc(s.config.id)}">Remove</button>
        </div>
      </div>
      <div class="mc-meta">
        <span class="req-prov">${tools} tool${tools === 1 ? "" : "s"}</span>
        ${s.server_info ? `<span>${esc(s.server_info)}</span>` : ""}
        <span title="${esc(detail)}">${esc(detail)}</span>
      </div>
      ${s.error ? `<div class="mem-item-body" style="color:var(--red); margin-top:6px;">${esc(s.error)}</div>` : ""}
    </div>`;
}

async function toolSourceAction(act, id) {
  try {
    if (act === "remove" && !confirm("Remove this tool source? Its tools will be disconnected.")) return;
    const res = await fetch(API + "/api/tools/sources/" + encodeURIComponent(id) + "/" + act, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    });
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    const labels = { connect: "connected", disconnect: "disconnected", remove: "removed" };
    toast("success", `✓ ${labels[act] || act} ${id}`);
    await loadToolSources();
    await loadTools(); // refresh the tool grid + count
  } catch (err) {
    toast("error", "Error: " + err.message);
  }
}

function onToolSourceTypeChange() {
  const type = $("#ts-type").value;
  $$(".ts-field").forEach((el) => { el.style.display = "none"; });
  $$(".ts-" + type).forEach((el) => { el.style.display = ""; });
}

async function submitToolSource(e) {
  e.preventDefault();
  const type = $("#ts-type").value;
  const name = $("#ts-name").value.trim();
  if (!name) { toast("error", "Name is required."); $("#ts-name")?.focus(); return; }
  const body = { name, type, auto_connect: $("#ts-autoconnect").checked, connect: true };
  if (type === "mcp_stdio") {
    body.command = $("#ts-command").value.trim();
    if (!body.command) { toast("error", "Command is required."); return; }
    const argsStr = $("#ts-args").value.trim();
    if (argsStr) body.args = argsStr.split(/\s+/);
  } else if (type === "mcp_http") {
    body.url = $("#ts-url").value.trim();
    if (!body.url) { toast("error", "URL is required."); return; }
    const hdrStr = $("#ts-headers").value.trim();
    if (hdrStr) { try { body.headers = JSON.parse(hdrStr); } catch { toast("error", "Headers must be valid JSON."); return; } }
  } else if (type === "internal") {
    body.path = $("#ts-path").value.trim();
    if (!body.path) { toast("error", "Path is required."); return; }
  }
  const btn = $("#ts-add-btn");
  if (btn) { btn.disabled = true; btn.textContent = "Connecting…"; }
  try {
    const res = await fetch(API + "/api/tools/sources", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
    if (data.warning) toast("warning", data.warning);
    else toast("success", `✓ Connected ${name}`);
    // Reset the form (keep the selected type).
    ["#ts-name", "#ts-command", "#ts-args", "#ts-url", "#ts-headers", "#ts-path"].forEach((s) => { const el = $(s); if (el) el.value = ""; });
    await loadToolSources();
    await loadTools();
  } catch (err) {
    toast("error", "Error: " + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = "⚡ Add & Connect"; }
  }
}

// ════════════════════════════════════════════════════════════════════════
