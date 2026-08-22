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
function connectSSE() {
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
    addEvent({ type: "connected", content: JSON.parse(e.data).status, timestamp: new Date().toISOString(), status: "connected" });
    // If an approval request arrived while the SSE was disconnected, pick it up now.
    pollPendingApprovals();
  });

  evtSource.onerror = () => {
    setConn("disconnected", "Reconnecting…");
    const el = $("#evt-live"); if (el) { el.textContent = "● PAUSED"; el.classList.add("paused"); }
  };

  // EventSource only delivers a named event to a listener registered for that
  // exact name, so this list is the whole contract: a type missing here is
  // broadcast by the server and dropped by the browser. Keep it in step with
  // core.EventType.
  const types = ["task_update","agent_spawn","agent_complete","tool_execution","model_route","compression","final_output","skill_extract","memory_store","dag_update","consensus","token_usage","error","approval","file_change","plan_updated","workflow_updated","chat_query","chat_response","sync_gui"];
  types.forEach((t) => {
    evtSource.addEventListener(t, (e) => {
      try {
        const data = JSON.parse(e.data);
        if (t === "token_usage") { handleTokenUsage(data); return; }
        if (t === "approval") { handleApprovalEvent(data); return; }
        if (t === "sync_gui") { handleSyncGUI(); return; }
        // Auto Mode announces a detected project as a task_update, not an
        // event type of its own — server/chat_handler.go emits
        // EmitTaskUpdate("project_auto_created", proj.ID, proj.Name), so the
        // id arrives in `status` and the name in `content`. This used to
        // listen for a "project_auto_created" event that the server never
        // sends, so Auto Mode never activated the project it had just found.
        if (t === "task_update" && data.task_id === "project_auto_created") {
          toast("success", "Auto Mode detected a project! Activating " + data.content);
          setActiveProject(data.status);
          switchTab("blueprint");
          return;
        }
        if (t === "plan_updated" && data.task_id === activeProjectId) {
          const v = $("#workflow-plan-view");
          if (v) renderPlanBoard(data.content || "", v, "plan");
          return;
        }
        if (t === "workflow_updated" && data.task_id === activeProjectId) {
          const v = $("#workflow-arch-view");
          if (v) renderPlanBoard(data.content || "", v, "workflow");
          return;
        }
        addEvent(data);
        if (t === "compression") {
          toast("info", "Context Compressed: " + data.content);
        }
        if (t === "tool_execution") { handleFileToolEvent(data); }
        // final_output / chat_response rendering is handled by the V2 event
        // router (220-v2.js) via finalizeAssistantMessage + the fetch path
        // in 50-chat.js. The old activeTab==="studio" branch here was dead
        // (activeTab is never "studio" after the nav refactor → nexus) and
        // would have created duplicate messages; removed.
      } catch (err) { /* ignore */ }
    });
  });
}

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
