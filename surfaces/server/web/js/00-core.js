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
// One entry per page, in palette order. With the section dropdown gone this
// is the only place a page's title and icon are written down: the palette is
// the only way to change page, and the title bar reads its label from here.
const NAV_META = {
  studio:     { ico: "💬",  label: "Studio (Chat & Workspace)" },
  blueprint:  { ico: "📐",  label: "Blueprint (Plan & Workflow)" },
  telemetry:  { ico: "📡",  label: "Telemetry" },
  knowledge:  { ico: "🧠",  label: "Knowledge" },
  projects:   { ico: "📁",  label: "Projects" },
  config:     { ico: "⚙️",  label: "Configuration & Models" },
};

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

  const meta = NAV_META[tab] || { ico: "", label: tab };
  $("#page-title").textContent = meta.label;

  // ── Per-tab data hydration ─────────────────────────────────────────
  // One page per palette heading; a page with sub-tabs hydrates every
  // sub-panel's data on entry (same as before Phase 9 — the status page's
  // three panels already worked this way), because sub-tab switching
  // (271/272/273/274-*-subtabs.js) is visibility-only and never fetches.
  if (tab === "projects" && $("#proj-grid") && !$("#proj-grid").children.length) loadProjects();

  if (tab === "knowledge") {
    if ($("#tools-grid") && !$("#tools-grid").children.length) loadTools();
    // Memory fetches counts (+ learned strategies, cached) only; the results
    // list stays empty until you ask it something or pick a tier.
    if ($("#mem-counts") && !$("#mem-counts").children.length) loadMemory();
  }

  if (tab === "telemetry") {
    hideEvtBadge();
    loadMetrics();
    loadAudit();
  }

  if (tab === "config") {
    if ($("#config-content") && !$("#config-content").dataset.loaded) loadConfig();
    loadCapabilities();
  }

  // Blueprint: fetch the active project's plan + workflow so the task board is
  // never stuck on the placeholder, even if the project was activated before
  // this tab was shown. Decisions (formerly the separate Cognition Cascade
  // tab) loads once at boot via 260-cascade.js's own MutationObserver init,
  // same as before — no per-tab fetch needed for it here.
  if (tab === "blueprint" && activeProjectId) fetchProjectPlanAndWorkflow(activeProjectId);

  // Studio: keep the workspace file tree live.
  if (tab === "studio") { loadFileTree(); startFileTreePoll(); }
  else stopFileTreePoll();

  // Charts only tick while you are looking at them.
  if (tab === "telemetry") startMetricsPolling(); else stopMetricsPolling();
}

// ════════════════════════════════════════════════════════════════════════
