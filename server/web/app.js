/* ════════════════════════════════════════════════════════════════════════
   DARKCODE-GO ENTERPRISE CONSOLE — Frontend Logic (module loader)
   ════════════════════════════════════════════════════════════════════════
   This was previously a single 5012-line app.js. It has been split into
   focused modules under /js/, loaded by the ordered <script> tags in
   index.html. The split preserves backward compatibility:
     • every function remains a global (no module/import),
     • inline handlers and the V2 window.addEvent/switchTab wraps keep working,
     • the go:embed web/* directive still embeds everything (zero build step).
   The modules attach to window.DC where new namespaced APIs exist (widgets).
   ════════════════════════════════════════════════════════════════════════ */
"use strict";

// Modules are loaded via <script> tags in dependency order (see index.html):
//   00-core.js      — globals, state, splash, tab nav
//   10-sse.js       — SSE connection + event dispatch
//   20-approvals.js — permission popup
//   30-tokens.js    — token telemetry + metrics polling
//   40-events.js    — live event feed rendering
//   50-chat.js      — chat send/receive/streaming
//   60-filetree.js  — workspace explorer + agent mod tracking
//   70-attachments.js — @-mention + attachment picker
//   80-tools.js     — tool registry + sources
//   90-memory.js    — 6-tier memory viewer
//   100-projects.js — projects + plan/workflow
//   110-status.js   — system telemetry
//   120-metrics.js  — monitoring dashboard + charts
//   130-config.js   — provider/model config + registered models
//   140-ui.js       — toast + command palette + helpers
//   150-events.js   — DOM event wiring (attachEventListeners)
//   160-widgets.js  — unified custom dropdowns/autocomplete
//   170-init.js     — init() bootstrap
//   180-dirpicker.js— directory picker
//   190-model-fetch.js — dynamic model fetching (enriched)
//   220-v2.js       — V2 execution pipeline / consensus / resource monitor
//   230-tail.js     — cancelChatExecution + reopenApprovalModal
//
// init() is called at the bottom of 170-init.js. No further bootstrap here.
// init() is called here (after every module has loaded) so all globals are
// defined before bootstrap runs.
if (typeof init === "function") init();

console.log("[darkcode] frontend modules loaded");
