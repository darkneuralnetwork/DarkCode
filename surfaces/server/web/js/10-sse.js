/* 10-sse.js — extracted from app.js (lines 186-334) */
// CONNECTION STATUS
// ════════════════════════════════════════════════════════════════════════
function setConn(state, label) {
  const el = $("#conn-indicator");
  if (!el) return;
  el.classList.remove("connected", "disconnected");
  if (state) el.classList.add(state);
  const lbl = el.querySelector(".conn-label");
  if (lbl) lbl.textContent = label;
}

// ════════════════════════════════════════════════════════════════════════
// SSE EVENT STREAM
// ════════════════════════════════════════════════════════════════════════
async function connectSSE() {
  if (evtSource) evtSource.close();
  try {
    evtSource = new EventSource(API + "/api/events");
  } catch (e) {
    setConn("disconnected", "SSE unsupported");
    return;
  }

  evtSource.addEventListener("connected", (e) => {
    setConn("connected", "Live");
    const el = $("#evt-live"); if (el) { el.textContent = "● LIVE"; el.classList.remove("paused"); }
    EventBus.emit("connected", { type: "connected", content: JSON.parse(e.data).status, timestamp: new Date().toISOString(), status: "connected" });
    // If an approval request arrived while the SSE was disconnected, pick it up now.
    pollPendingApprovals();
  });

  evtSource.onerror = () => {
    setConn("disconnected", "Reconnecting…");
    const el = $("#evt-live"); if (el) { el.textContent = "● PAUSED"; el.classList.add("paused"); }
  };

  // EventSource only delivers a named event to a listener registered for that
  // exact name, so this list is the whole contract: a type missing here is
  // broadcast by the server and dropped by the browser. Sourced from
  // /api/event-types (06-eventtypes.js) rather than hardcoded here — that
  // registry is generated from core.EventType itself, so it can't go stale
  // the way a second hand-written copy already had (see event_meta.go).
  //
  // This callback is now pure transport: parse the frame, fan it out on
  // EventBus, done. Every type-specific reaction (token meters, approval
  // popup, GUI resync, Auto Mode project detection, the live plan/workflow
  // board, the raw feed, the exec-status-bar) is a listener registered near
  // where it's defined, not a branch inlined here — see EventBus.on calls in
  // 20-approvals.js, 30-tokens.js, 40-events.js, 60-filetree.js,
  // 100-projects.js, 220-v2.js, 260-cascade.js, and this file (below, for
  // sync_gui and the small "compression" toast that has no better home).
  const eventTypes = await getEventTypes();
  const types = eventTypes.map((t) => t.type);
  types.forEach((t) => {
    evtSource.addEventListener(t, (e) => {
      try {
        EventBus.emit(t, JSON.parse(e.data));
      } catch (err) { /* ignore */ }
    });
  });
}

EventBus.on("sync_gui", handleSyncGUI);
EventBus.on("compression", (data) => toast("info", "Context Compressed: " + data.content));

async function handleSyncGUI() {
  // Clear the chat container to prevent duplicates
  const msgs = $("#chat-messages");
  if (msgs) {
    const empty = msgs.querySelector(".chat-empty");
    msgs.innerHTML = "";
    if (empty) msgs.appendChild(empty);
  }

  // Refetch history and state
  try {
    const [histRes, stateRes] = await Promise.allSettled([
      fetch(API + "/api/events/history"),
      fetch(API + "/api/session/state")
    ]);

    if (stateRes.status === "fulfilled" && stateRes.value.ok) {
      const state = await stateRes.value.json();
      if (state.active_project && state.active_project !== activeProjectId) {
        setActiveProject(state.active_project);
      }
    }

    if (histRes.status === "fulfilled" && histRes.value.ok) {
      const hist = await histRes.value.json();
      if (hist && hist.events && Array.isArray(hist.events)) {
        hist.events.forEach(evt => {
          if (evt.type === "chat_query") appendMsg("user", String(evt.content || ""));
          if (evt.type === "chat_response") appendMsg("assistant", String(evt.content || ""));
        });
      }
    }
  } catch (err) {
    console.error("Failed to sync GUI state", err);
  }
}

// ════════════════════════════════════════════════════════════════════════
