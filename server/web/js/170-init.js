/* 170-init.js — extracted from app.js (lines 4047-4152) */
// INIT
// ════════════════════════════════════════════════════════════════════════
async function loadComponents() {
  const panels = $$(".tab-panel[data-src]");
  await Promise.all(Array.from(panels).map(async (panel) => {
    try {
      const res = await fetch(panel.dataset.src);
      if (res.ok) panel.innerHTML = await res.text();
    } catch(err) {
      console.error("Failed to load component:", panel.dataset.src, err);
    }
  }));
}

async function init() {
  // Start splash animation in parallel
  const splashPromise = runSplash();

  await loadComponents();
  if (window.DC && window.DC.scanWidgets) window.DC.scanWidgets(document); // unified custom dropdowns after fragments loaded

  // Fault-isolate listener attachment: a single bad addEventListener must
  // not abort the rest of init() (it previously did, via the `$(window)` bug,
  // which left connectSSE()/loadConfig() unrun). Each section below is also
  // independent, but this guard guarantees the network hydration still runs.
  try { attachEventListeners(); } catch (e) { console.error("attachEventListeners failed:", e); }

  // Start SSE immediately — it only needs the DOM (not the data fetches) so
  // live events begin arriving ASAP. Also kick off the file-tree bootstrap
  // in parallel (only needs the DOM element).
  connectSSE();
  if ($("#fe-tree")) { loadFileTree(); startFileTreePoll(); }

  // Parallel hydration: fire ALL data fetches concurrently instead of the
  // old serial chain (session → providers → status+history → config). This
  // collapses 4 sequential round-trips into 1, cutting cold-start latency.
  const [sessionRes, providersRes, statusRes, histRes] = await Promise.allSettled([
    fetch(API + "/api/session/state"),
    (async () => { await loadProviders(); })(),
    fetch(API + "/api/status"),
    fetch(API + "/api/events/history"),
  ]);

  // Restore the previously-activated project (from backend or localStorage).
  try {
    if (sessionRes.status === "fulfilled" && sessionRes.value.ok) {
      const state = await sessionRes.value.json();
      if (state.active_project) {
        setActiveProject(state.active_project);
      } else {
        const savedProject = localStorage.getItem("darkcode_active_project");
        if (savedProject) setActiveProject(savedProject);
      }
    } else {
      const savedProject = localStorage.getItem("darkcode_active_project");
      if (savedProject) setActiveProject(savedProject);
    }
  } catch (e) {
    const savedProject = localStorage.getItem("darkcode_active_project");
    if (savedProject) setActiveProject(savedProject);
  }

  try {
    // Status → header badges + meters + connection indicator.
    if (statusRes.status === "fulfilled" && statusRes.value.ok) {
      const d = await statusRes.value.json();
      updateBadges(d);
      updateMetersFromSnap(d.metrics || {});
      // The API is up — reflect that immediately rather than waiting for the
      // SSE "connected" event (which may be delayed). connectSSE() keeps the
      // indicator live and flips it to "Reconnecting…" on drop.
      setConn("connected", "Live");
    } else {
      setConn("disconnected", "API offline");
    }

    // Hydrate configuration (form + model cards + topbar switcher). Runs in
    // parallel with the status/history fetches above; awaited here so the
    // model switcher is populated before the splash hides.
    await loadConfig();

    // Historical events → replay into the (now coalescing) event feed + chat.
    if (histRes.status === "fulfilled" && histRes.value.ok) {
      const hist = await histRes.value.json();
      if (hist && hist.events && Array.isArray(hist.events)) {
        hist.events.forEach(evt => {
          addEvent(evt);
          if (evt.type === "chat_query") appendMsg("user", String(evt.content || ""));
          if (evt.type === "chat_response") appendMsg("assistant", String(evt.content || ""));
        });
      }
    }
  } catch (err) {
    setConn("disconnected", "API offline");
  }

  // Wait for splash to finish before hiding the overlay.
  await splashPromise;
  
  // Ensure the default tab is active
  switchTab("telemetry");
}

// init() is invoked from app.js (the last-loaded module) so that every
// module's globals (incl. cancelChatExecution / reopenApprovalModal in
// 230-tail.js) are defined before init() runs.

// ════════════════════════════════════════════════════════════════════════
