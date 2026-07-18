/* 140-ui.js — extracted from app.js (lines 3095-3276) */
// TOAST NOTIFICATIONS
// ════════════════════════════════════════════════════════════════════════
function toast(type, message, duration) {
  duration = duration || 4000;
  const container = $("#toast-container");
  if (!container) return;
  const t = document.createElement("div");
  t.className = "toast " + type;
  const icons = { success: "✓", error: "✗", warning: "⚠", info: "ℹ" };
  t.innerHTML = `<span class="toast-icon">${icons[type] || "ℹ"}</span><span class="toast-msg">${esc(message)}</span>`;
  container.appendChild(t);
  setTimeout(() => {
    t.classList.add("removing");
    setTimeout(() => t.remove(), 300);
  }, duration);
}

// ════════════════════════════════════════════════════════════════════════
// COMMAND PALETTE
// ════════════════════════════════════════════════════════════════════════
const cmdItems = [
  { group: "Navigate", icon: "💬", label: "Studio", hint: "studio", action: () => switchTab("studio") },
  { group: "Navigate", icon: "📡", label: "Live Events", hint: "events", action: () => switchTab("events") },
  { group: "Navigate", icon: "📊", label: "Monitoring Dashboard", hint: "monitor", action: () => switchTab("monitoring") },
  { group: "Navigate", icon: "⚙️", label: "Configuration & Models", hint: "config", action: () => switchTab("config") },
  { group: "Navigate", icon: "🔧", label: "Tool Registry", hint: "tools", action: () => switchTab("tools") },
  { group: "Navigate", icon: "🧠", label: "6-Tier Memory", hint: "memory", action: () => switchTab("memory") },
  { group: "Navigate", icon: "📁", label: "Projects", hint: "projects", action: () => switchTab("projects") },
  { group: "Navigate", icon: "🔵", label: "System Telemetry", hint: "status", action: () => switchTab("status") },
  { group: "Actions", icon: "⚡", label: "Add LLM Model", hint: "add model", action: () => { switchTab("config"); setTimeout(() => { const addBtn = document.querySelector('#cfg-mc-toggle .cfg-mc-btn[data-view="add"]'); if (addBtn) addBtn.click(); $("#cfg-provider")?.focus(); }, 300); } },
  { group: "Actions", icon: "🔄", label: "Refresh Metrics", hint: "refresh", action: () => { loadMetrics(); toast("info", "Metrics refreshed"); } },
  { group: "Actions", icon: "🗑️", label: "Reset Metrics", hint: "reset", action: () => { $("#mon-reset")?.click(); } },
  { group: "Actions", icon: "🧹", label: "Clear Events", hint: "clear events", action: () => { $("#evt-clear")?.click(); } },
  { group: "Actions", icon: "💬", label: "Focus Chat Input", hint: "focus chat", action: () => { switchTab("studio"); setTimeout(() => $("#chat-text")?.focus(), 200); } },
  { group: "Actions", icon: "📎", label: "Attach File or Folder", hint: "attach file", action: () => { switchTab("studio"); setTimeout(() => { $("#chat-text")?.focus(); openAttachBrowser("button", $("#chat-attach")); }, 200); } },
];

let cmdSelectedIdx = 0;

function openCmdPalette() {
  const p = $("#cmd-palette");
  if (!p) return;
  p.classList.add("open");
  const input = $("#cmd-input");
  input.value = "";
  renderCmdResults("");
  setTimeout(() => input.focus(), 50);
}

function closeCmdPalette() {
  const p = $("#cmd-palette");
  if (p) p.classList.remove("open");
}

function renderCmdResults(query) {
  const container = $("#cmd-results");
  if (!container) return;
  const q = query.toLowerCase().trim();
  let filtered = cmdItems;
  if (q) {
    filtered = cmdItems.filter(item =>
      item.label.toLowerCase().includes(q) || item.hint.toLowerCase().includes(q) || item.group.toLowerCase().includes(q)
    );
  }
  cmdSelectedIdx = 0;
  if (!filtered.length) {
    container.innerHTML = '<div class="mem-empty" style="padding:24px">No matching commands</div>';
    return;
  }
  let html = "";
  let lastGroup = "";
  filtered.forEach((item, i) => {
    if (item.group !== lastGroup) {
      html += `<div class="cmd-group">${esc(item.group)}</div>`;
      lastGroup = item.group;
    }
    html += `<div class="cmd-item ${i === 0 ? 'selected' : ''}" data-idx="${i}">
      <span class="cmd-item-icon">${item.icon}</span>
      <span class="cmd-item-label">${esc(item.label)}</span>
      <span class="cmd-item-hint">${esc(item.hint)}</span>
    </div>`;
  });
  container.innerHTML = html;
  // Store filtered items for keyboard nav
  container.dataset.filtered = JSON.stringify(filtered.map(f => cmdItems.indexOf(f)));
  container.querySelectorAll(".cmd-item").forEach(el => {
    el.addEventListener("click", () => {
      const idx = parseInt(el.dataset.idx, 10);
      const filteredArr = getFilteredCmds();
      if (filteredArr[idx]) { filteredArr[idx].action(); closeCmdPalette(); }
    });
    el.addEventListener("mouseenter", () => {
      container.querySelectorAll(".cmd-item").forEach(e => e.classList.remove("selected"));
      el.classList.add("selected");
      cmdSelectedIdx = parseInt(el.dataset.idx, 10);
    });
  });
}

function getFilteredCmds() {
  const container = $("#cmd-results");
  if (!container || !container.dataset.filtered) return cmdItems;
  try {
    const indices = JSON.parse(container.dataset.filtered);
    return indices.map(i => cmdItems[i]);
  } catch { return cmdItems; }
}

function moveCmdSelection(dir) {
  const container = $("#cmd-results");
  const items = container.querySelectorAll(".cmd-item");
  if (!items.length) return;
  cmdSelectedIdx = (cmdSelectedIdx + dir + items.length) % items.length;
  items.forEach((el, i) => el.classList.toggle("selected", i === cmdSelectedIdx));
  items[cmdSelectedIdx].scrollIntoView({ block: "nearest" });
}

function execCmdSelection() {
  const filtered = getFilteredCmds();
  if (filtered[cmdSelectedIdx]) { filtered[cmdSelectedIdx].action(); closeCmdPalette(); }
}

// ════════════════════════════════════════════════════════════════════════
// HELPERS
// ════════════════════════════════════════════════════════════════════════
function esc(s) {
  const d = document.createElement("div");
  d.textContent = s == null ? "" : s;
  return d.innerHTML;
}

// safeUrl vets a markdown link target before it becomes an <a href>. It blocks
// dangerous schemes (javascript:, data:, vbscript:, file:) that would execute
// on click — a DOM-XSS vector since chat/tool output (including fetched web
// pages) can contain attacker-controlled markdown like [x](javascript:...).
// The input here is already HTML-escaped by renderMarkdown's esc() pass, so we
// only need to reason about the scheme. Anything not clearly safe becomes "#".
function safeUrl(url) {
  const raw = String(url == null ? "" : url).trim();
  // Relative, anchor, and protocol-relative-safe cases: no scheme means it
  // inherits the page origin, which is fine.
  // Reject control chars/whitespace an attacker might use to smuggle a scheme
  // past the check (e.g. "java\tscript:"). esc() already neutralizes quotes.
  const stripped = raw.replace(/[\x00-\x20]+/g, "");
  const m = /^([a-z][a-z0-9+.-]*):/i.exec(stripped);
  if (!m) return raw; // no scheme → relative/anchor, allowed
  const scheme = m[1].toLowerCase();
  if (scheme === "http" || scheme === "https" || scheme === "mailto") return raw;
  return "#";
}

function renderMarkdown(text) {
  if (!text) return "";
  let html = esc(text);
  // Code blocks: ```language\ncode\n```
  html = html.replace(/```(\w*)\n([\s\S]*?)```/g, (match, lang, code) => {
    if (lang === "mermaid") {
      return `<div class="mermaid">${code}</div>`;
    }
    return `<pre class="md-code-block"><code class="language-${lang}">${code}</code></pre>`;
  });
  // Inline code: `code`
  html = html.replace(/`([^`]+)`/g, `<code class="md-inline-code">$1</code>`);
  // Bold: **text**
  html = html.replace(/\*\*([^*]+)\*\*/g, `<strong>$1</strong>`);
  // Italics: *text*
  html = html.replace(/\*([^*]+)\*/g, `<em>$1</em>`);
  // Links: [text](url) — sanitize the href scheme so a javascript:/data: URL
  // in model or fetched-page output cannot execute on click (DOM-XSS guard).
  html = html.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (m, label, url) =>
    `<a href="${safeUrl(url)}" target="_blank" rel="noopener noreferrer">${label}</a>`);
  // Line breaks
  html = html.replace(/\n/g, "<br>");
  return html;
}
function fmtTime(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  if (isNaN(d)) return "";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}
function fmtTimeShort(ts) {
  if (!ts) return "—";
  const d = new Date(ts);
  if (isNaN(d)) return "—";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}
function fmtNum(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n/1e9).toFixed(2)+"B";
  if (n >= 1e6) return (n/1e6).toFixed(2)+"M";
  if (n >= 1e3) return (n/1e3).toFixed(1)+"k";
  return String(n);
}
function fmtCost(c) {
  c = Number(c) || 0;
  if (c === 0) return "$0.00";
  if (c < 0.01) return "$"+c.toFixed(4);
  return "$"+c.toFixed(2);
}
function fmtDur(ms) {
  ms = Number(ms) || 0;
  if (ms < 1000) return ms + "ms";
  return (ms/1000).toFixed(1) + "s";
}

// ════════════════════════════════════════════════════════════════════════
