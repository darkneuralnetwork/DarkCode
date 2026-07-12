/* 00-core.js — extracted from app.js (lines 1-185) */
/* ════════════════════════════════════════════════════════════════════════
   DARKCODE-GO ENTERPRISE CONSOLE — Frontend Logic
   ════════════════════════════════════════════════════════════════════════ */
"use strict";

const API = "";
const $  = (sel) => document.querySelector(sel);
const $$ = (sel) => document.querySelectorAll(sel);

// ---------- State ----------
let evtSource = null;
let evtCount = 0;
let activeTab = "studio";
let activeProjectId = null;   // project whose context.md is injected into chat
let activeProjectName = "";   // cached name of the active project (for the chat banner)
let activeContextLen = 0;     // cached length of the active project's context.md
let pendingChatAnswer = "";   // captures the assistant's last answer for context sync
let projEditingId = null;     // project id being edited in the modal (null = creating)
let ctxEditingId = null;      // project id whose context is open in the editor
let providerCatalog = [];
let metricsPollTimer = null;
let charts = {};
let metricsRefreshPending = false;
let sparklineData = { tokens: [], cost: [], reqs: [] };
const SPARK_MAX = 30;

// ---------- Chart palette ----------
const C = {
  orange: "#ff6b00",
  amber: "#ff9100",
  blue: "#29b6f6",
  green: "#00e676",
  yellow: "#ffd54f",
  red: "#ff5252",
  purple: "#ba68c8",
  cyan: "#26c6da",
  dim: "#9090a0",
  grid: "rgba(255,255,255,0.04)",
  text: "#e8e8f0",
};
const MODEL_COLORS = [C.orange, C.blue, C.green, C.yellow, C.purple, C.amber, C.red, C.cyan, "#f472b6", "#84cc16"];

// ════════════════════════════════════════════════════════════════════════
// SPLASH SCREEN
// ════════════════════════════════════════════════════════════════════════
const splashStatuses = [
  "Initializing kernel…",
  "Loading 7-layer architecture…",
  "Registering tools…",
  "Connecting to LLM providers…",
  "Wiring memory system…",
  "Starting event stream…",
  "Ready."
];

async function runSplash() {
  const statusEl = $("#splash-status");
  for (const s of splashStatuses) {
    if (statusEl) statusEl.textContent = s;
    await sleep(180 + Math.random() * 120);
  }
  await sleep(200);
  const splash = $("#splash");
  if (splash) splash.classList.add("hidden");
}

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

// ════════════════════════════════════════════════════════════════════════
// TAB NAVIGATION
// ════════════════════════════════════════════════════════════════════════
// Section metadata for the header dropdown: the trigger button always shows
// the active section's icon + label.
const NAV_META = {
  // Consolidated tabs (the active nav in index.html). These were previously
  // missing, which made switchTab fall back to {ico:"", label:titles[tab]} —
  // the trigger icon vanished on every consolidated-tab switch.
  nexus:      { ico: "💬",  label: "Nexus (Chat & Workspace)" },
  blueprint:  { ico: "📐",  label: "Blueprint (Plan & Workflow)" },
  registry:   { ico: "🔧",  label: "Tool Registry" },
  cognition:  { ico: "🧠",  label: "Cognition" },
  telemetry:  { ico: "📊",  label: "Telemetry" },
  config:     { ico: "⚙️",  label: "Configurations" },
  // Legacy granular tabs (kept for command-palette / fallback compatibility).
  chat:       { ico: "💬",  label: "Chat Console" },
  events:     { ico: "📡",  label: "Live Events" },
  tools:      { ico: "🔧",  label: "Tool Registry" },
  memory:     { ico: "🧠",  label: "6-Tier Memory" },
  projects:   { ico: "📁",  label: "Projects" },
  workflow:   { ico: "🗺️", label: "Plan & Workflow" },
  knowledge:  { ico: "🕸️", label: "Knowledge Graph" },
  learning:   { ico: "📈",  label: "Learning Engine" },
  audit:      { ico: "🛡️",  label: "Audit & Governance" },
  monitoring: { ico: "📊",  label: "Monitoring" },
  status:     { ico: "🔵",  label: "System Telemetry" },
};

// Section dropdown wiring (trigger toggle + item select + outside/Esc close).
(function initNavDropdown() {
  const dropdown = document.getElementById("nav-dropdown");
  const btn = document.getElementById("nav-dropdown-btn");
  const menu = document.getElementById("nav-dropdown-menu");
  if (!dropdown || !btn || !menu) return;

  const closeMenu = () => {
    menu.hidden = true;
    btn.setAttribute("aria-expanded", "false");
  };
  const openMenu = () => {
    menu.hidden = false;
    btn.setAttribute("aria-expanded", "true");
    const act = menu.querySelector(".nav-dropdown-item.active");
    if (act) act.scrollIntoView({ block: "nearest" });
  };

  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    if (menu.hidden) openMenu(); else closeMenu();
  });
  menu.addEventListener("click", (e) => {
    const item = e.target.closest(".nav-dropdown-item");
    if (!item) return;
    switchTab(item.dataset.tab);
    closeMenu();
  });
  document.addEventListener("click", (e) => {
    if (!menu.hidden && !dropdown.contains(e.target)) closeMenu();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !menu.hidden) closeMenu();
  });
})();

function switchTab(tab) {
  activeTab = tab;
  closeAttachBrowser();
  $$(".tab-panel").forEach((p) => {
    if (p.id === "tab-" + tab) {
      p.classList.add("active");
      p.classList.remove("fade-in");
      void p.offsetWidth;
      p.classList.add("fade-in");
    } else {
      p.classList.remove("active");
    }
  });

  const titles = {
    nexus: "Nexus (Chat & Workspace)",
    blueprint: "Blueprint (Plan & Workflow)",
    registry: "Tool Registry",
    cognition: "Cognition",
    telemetry: "Telemetry",
    config: "Configurations"
  };
  const meta = NAV_META[tab] || { ico: "", label: titles[tab] || tab };
  const lblEl = document.getElementById("nav-dropdown-label");
  const icoEl = document.getElementById("nav-dropdown-icon");
  if (lblEl) lblEl.textContent = meta.label;
  if (icoEl) icoEl.textContent = meta.ico;
  $$(".nav-dropdown-item").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  // Close the dropdown menu if it was open.
  const menu = document.getElementById("nav-dropdown-menu");
  const btn = document.getElementById("nav-dropdown-btn");
  if (menu) menu.hidden = true;
  if (btn) btn.setAttribute("aria-expanded", "false");
  $("#page-title").textContent = titles[tab] || tab;

  // ── Per-tab data hydration ─────────────────────────────────────────
  // Tab names follow the consolidated nav (index.html): nexus, blueprint,
  // registry, cognition, telemetry, config. The old granular tabs
  // (memory/knowledge/projects/workflow/monitoring/studio/learning) were
  // merged; their loaders are now triggered from the consolidated tabs.
  if (tab === "registry" && $("#tools-grid") && !$("#tools-grid").children.length) loadTools();

  // Cognition now hosts Projects + Project Intelligence + 6-Tier Memory +
  // Knowledge Graph (formerly separate tabs). Hydrate each surface; each
  // loader is idempotent and only fetches when its container is empty.
  if (tab === "cognition") {
    if ($("#proj-grid") && !$("#proj-grid").children.length) loadProjects();
    if ($("#mem-conversation") && !$("#mem-conversation").children.length) loadMemory();
    if ($("#kg-nodes") && !$("#kg-nodes").children.length) loadKnowledgeGraph();
    if (typeof window.loadIntelligence === "function") window.loadIntelligence();
  }

  if (tab === "telemetry") {
    hideEvtBadge();
    if ($("#audit-list") && !$("#audit-list").children.length) loadAudit();
    if ($("#status-content") && !$("#status-content").children.length) loadStatus();
    loadMetrics();
    if (typeof renderConsensusHistory === "function") renderConsensusHistory();
  }
  if (tab === "config" && $("#config-content") && !$("#config-content").dataset.loaded) loadConfig();

  // Blueprint: hydrate the project list (moved from Cognition) + fetch the
  // active project's plan + workflow so the task board is never stuck on the
  // placeholder, even if the project was activated before this tab was shown.
  if (tab === "blueprint") {
    if ($("#proj-grid") && !$("#proj-grid").children.length) loadProjects();
    if (activeProjectId) fetchProjectPlanAndWorkflow(activeProjectId);
  }

  // Nexus (formerly "studio"/"chat"): keep the workspace file tree live.
  if (tab === "nexus") { loadFileTree(); startFileTreePoll(); }
  else stopFileTreePoll();

  // Metrics polling follows the consolidated Telemetry tab (monitoring was
  // merged into telemetry).
  if (tab === "telemetry") startMetricsPolling(); else stopMetricsPolling();
}

// ════════════════════════════════════════════════════════════════════════
