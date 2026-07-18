/* 150-events.js — extracted from app.js (lines 3277-3940) */
// EVENT WIRING
// ════════════════════════════════════════════════════════════════════════
function attachEventListeners() {
  // Chat/Build mode + Loop + Brain composer controls (Phase 5). These drive the
  // hidden #chat-mode-value / #chat-brain-value that the send body reads.
  //  • Chat  → chat_mode "general" (talk/Q&A, no tools) — Loop toggle hidden.
  //  • Build → chat_mode "project" (coding with tools); with Loop checked →
  //    "loop" (auto-generate + run tasks). Loop is only meaningful in Build.
  function syncMode() {
    const active = document.querySelector(".mode-seg-btn.active");
    const base = active ? active.dataset.mode : "project";
    const loopWrap = $("#chat-loop-wrap");
    const loopOn = $("#chat-loop-toggle")?.checked;
    if (loopWrap) loopWrap.style.display = base === "project" ? "inline-flex" : "none";
    const mv = $("#chat-mode-value");
    if (mv) mv.value = base === "project" && loopOn ? "loop" : base;
  }
  $$(".mode-seg-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      $$(".mode-seg-btn").forEach((b) => b.classList.toggle("active", b === btn));
      syncMode();
    });
  });
  $("#chat-loop-toggle")?.addEventListener("change", syncMode);
  $("#chat-brain-select")?.addEventListener("change", (e) => {
    const bv = $("#chat-brain-value");
    if (bv) bv.value = e.target.value || "auto";
  });
  syncMode();

  // Knowledge ingestion (Phase 3): teach the system from a file/dir/url/text.
  $("#cfg-ingest-btn")?.addEventListener("click", async () => {
    const input = $("#cfg-ingest-source");
    const status = $("#cfg-ingest-status");
    const source = (input?.value || "").trim();
    if (!source) { toast("info", "Enter a path, URL, or text to ingest."); return; }
    const btn = $("#cfg-ingest-btn");
    const label = btn.textContent;
    btn.disabled = true; btn.textContent = "Ingesting…";
    if (status) { status.style.display = "block"; status.textContent = "Ingesting…"; }
    try {
      const res = await fetch(API + "/api/ingest", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source }),
      });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || "ingest failed");
      let msg = `✓ ${d.sources} source(s) → ${d.chunks} memory chunk(s)`;
      if (d.kg_nodes) msg += `, ${d.kg_nodes} code nodes`;
      if (d.skipped) msg += `, ${d.skipped} skipped`;
      if (status) { status.style.color = "var(--green, #34d399)"; status.textContent = msg; }
      toast("success", "Knowledge ingested.");
      if (input) input.value = "";
    } catch (err) {
      if (status) { status.style.color = "var(--red, #f87171)"; status.textContent = "✗ " + err.message; }
      toast("error", err.message);
    } finally {
      btn.disabled = false; btn.textContent = label;
    }
  });

  // "Browse" button → reuse the directory picker to fill the ingest input with a
  // filesystem path (directories). Files can be selected via the Content browser.
  $("#cfg-ingest-browse")?.addEventListener("click", () => {
    if (typeof openDirPicker === "function") openDirPicker("cfg-ingest-source");
  });

  // Collapsible "Content" browser: lists the files/subdirs at the entered path
  // (or the workspace when empty), doubling as a picker — click a folder to
  // drill in, click a file to select it into the ingest input.
  async function loadIngestContent(path) {
    const bc = $("#cfg-ingest-breadcrumb");
    const list = $("#cfg-ingest-listing");
    if (!list) return;
    list.innerHTML = `<div style="padding:8px; color:var(--text-mute); font-size:12px;">Loading…</div>`;
    try {
      const q = path ? "?path=" + encodeURIComponent(path) : "";
      const res = await fetch(API + "/api/workspace/browse" + q);
      if (!res.ok) throw new Error(await res.text());
      const d = await res.json();
      const cwd = d.cwd || "";
      if (bc) bc.textContent = cwd || "(workspace)";
      const rowStyle = "display:flex; align-items:center; gap:8px; padding:5px 8px; cursor:pointer; font-size:12px; border-radius:4px;";
      let html = "";
      if (d.parent) {
        html += `<div class="ing-row" data-dir="${esc(d.parent)}" style="${rowStyle}"><span>📁</span><span>..</span></div>`;
      }
      (d.entries || []).forEach((e) => {
        const abs = cwd ? cwd.replace(/\/$/, "") + "/" + e.name : e.name;
        if (e.is_dir || e.isDir) {
          html += `<div class="ing-row" data-dir="${esc(abs)}" style="${rowStyle}"><span>📁</span><span>${esc(e.name)}</span></div>`;
        } else {
          html += `<div class="ing-row" data-file="${esc(abs)}" style="${rowStyle}"><span>📄</span><span>${esc(e.name)}</span></div>`;
        }
      });
      if (!html) html = `<div style="padding:8px; color:var(--text-mute); font-size:12px;">Empty.</div>`;
      // A "use this folder" affordance at the top.
      if (cwd) html = `<div class="ing-row" data-usedir="${esc(cwd)}" style="${rowStyle} color:var(--accent-3);"><span>✓</span><span>Use this folder for ingest</span></div>` + html;
      list.innerHTML = html;
    } catch (err) {
      list.innerHTML = `<div style="padding:8px; color:var(--red,#f87171); font-size:12px;">${esc(err.message)}</div>`;
    }
  }
  $("#cfg-ingest-content-toggle")?.addEventListener("click", () => {
    const panel = $("#cfg-ingest-content");
    const toggle = $("#cfg-ingest-content-toggle");
    if (!panel) return;
    const open = panel.style.display !== "none";
    panel.style.display = open ? "none" : "block";
    if (toggle) toggle.textContent = (open ? "▸" : "▾") + " Content";
    if (!open) loadIngestContent(($("#cfg-ingest-source")?.value || "").trim());
  });
  $("#cfg-ingest-listing")?.addEventListener("click", (e) => {
    const row = e.target.closest(".ing-row");
    if (!row) return;
    if (row.dataset.dir) { loadIngestContent(row.dataset.dir); return; }
    const pick = row.dataset.file || row.dataset.usedir;
    if (pick) {
      const input = $("#cfg-ingest-source");
      if (input) input.value = pick;
      toast("info", "Selected: " + pick);
    }
  });

  // Global modal handlers
  document.addEventListener("click", (e) => {
    // Maximize button
    if (e.target.matches(".max-btn")) {
      const targetId = e.target.dataset.target;
      const targetEl = document.getElementById(targetId);
      const titleEl = e.target.closest(".glass-panel")?.querySelector(".cfg-h3, .fe-title") || e.target.closest("[class$='-layout']")?.querySelector("h2");
      if (!targetEl) return;
      
      const modal = $("#maximize-modal");
      const modalBody = $("#maximize-modal-body");
      const modalTitle = $("#maximize-modal-title");
      
      modalBody.innerHTML = "";
      // Clone the content
      const clone = targetEl.cloneNode(true);
      // Remove any max-btn in the clone
      clone.querySelectorAll(".max-btn").forEach(b => b.remove());
      clone.style.height = "100%";
      clone.style.overflow = "auto";
      clone.style.margin = "0";
      
      modalBody.appendChild(clone);
      modalTitle.textContent = titleEl ? titleEl.textContent : "Maximized View";
      modal.style.display = "flex";
    }
    // File viewer minimize
    if (e.target.id === "file-viewer-min-btn") {
      $("#file-viewer-modal").style.display = "none";
    }
    // File viewer close
    if (e.target.id === "file-viewer-close-btn") {
      $("#file-viewer-modal").style.display = "none";
    }
    // Maximize modal close
    if (e.target.id === "maximize-close-btn") {
      $("#maximize-modal").style.display = "none";
    }
    // Provider Config Modal close
    if (e.target.id === "provider-modal-close-btn") {
      $("#provider-modal").style.display = "none";
    }
    // Close modals on clicking overlay background
    if (e.target.classList.contains("perm-overlay")) {
      e.target.style.display = "none";
    }
    // Close custom dropdowns if clicking outside
    if (!e.target.closest(".custom-select-container")) {
      document.querySelectorAll(".custom-select-container").forEach(c => c.classList.remove("open"));
    }
  });

  $("#evt-clear")?.addEventListener("click", () => {
    const list = $("#events-list");
    if (list) list.innerHTML = '<div class="events-empty">Awaiting event stream…</div>';
    evtCount = 0;
    lastGroupEl = null;
    const c = $("#evt-count"); if (c) c.textContent = "0";
    hideEvtBadge();
  });

  $("#chat-form")?.addEventListener("submit", (e) => {
    e.preventDefault();
    sendChat();
  });

  // New Chat: save & close the active project (its context is already
  // auto-saved on each exchange), reset STM + permissions, switch to General
  // mode, and clear the transcript. This is the "start fresh" action —
  // distinct from chat-clear (keeps memory) and chat-reset (clears memory
  // but leaves the project + mode alone).
  // New Chat — mode-aware:
  //  • General mode → clear transcript + reset STM (remove context).
  //  • Project/Loop/Auto mode → deactivate the active project + open the
  //    new-project modal so the user can start a fresh project. Keeps the
  //    current mode (doesn't force General). Previously this always switched
  //    to General mode, which is why it appeared "not working" in project mode.
  //  • Project/Loop/Auto with no active project → just open the new-project
  //    modal.
  // Blocked while a response is in flight (pendingChat is the real tracker;
  // the old code referenced an undefined `sendDisabled` which was a dead guard).
  // New Chat — the single "start fresh" control, uniform across every mode.
  // It always: (1) in project/loop mode, deactivates the active project so its
  // plan/workflow stops being injected; (2) POSTs /api/reset, which clears STM
  // AND advances the session epoch server-side so prior conversations no longer
  // resurface through memory recall; (3) clears the transcript. Distinct from
  // Clear Screen (Ctrl+L), which only hides the transcript and keeps memory.
  $("#chat-new")?.addEventListener("click", async () => {
    if (pendingChat) { toast("info", "Wait for the current response to finish."); return; }
    const mode = $("#chat-mode-value")?.value || "general";
    try {
      // Drop the active project first (project/loop) so its long-lived context
      // isn't carried into the fresh chat.
      if (mode !== "general" && activeProjectId) {
        await setActiveProject(null);
      }
      // Wipe conversational context in every mode (STM + session epoch).
      const res = await fetch(API + "/api/reset", { method: "POST" });
      if (!res.ok) throw new Error("Reset failed");
      const msgs = $("#chat-messages");
      if (msgs) {
        msgs.innerHTML = `
          <div class="chat-empty">
            <div class="chat-empty-icon pulse-anim">⚡</div>
            <h3>New Chat</h3>
            <p>Fresh session · previous context cleared. Type to begin.</p>
          </div>`;
      }
      toast("success", "New chat started — previous context cleared.");
    } catch (err) {
      toast("error", err.message);
    }
  });

  // Clear Screen (Ctrl+L) — purely cosmetic in EVERY mode: it hides the visible
  // transcript but never touches memory or the session. This removes the old
  // overlap with New Chat (which is the one control that actually resets); use
  // New Chat to start fresh, Clear Screen to just tidy the view.
  $("#chat-clear")?.addEventListener("click", () => {
    const msgs = $("#chat-messages");
    if (!msgs) return;
    msgs.innerHTML = `
      <div class="chat-empty">
        <div class="chat-empty-icon pulse-anim">⚡</div>
        <h3>Screen Cleared</h3>
        <p>Transcript hidden — memory and context are preserved. Type to continue,
        or use New Chat to start fresh.</p>
      </div>
    `;
    msgs.scrollTop = 0;
    toast("info", "Screen cleared (memory kept).");
  });


  $("#chat-text")?.addEventListener("keydown", onChatTextKeydown);
  $("#chat-text")?.addEventListener("input", function () {
    this.style.height = "auto"; this.style.height = Math.min(this.scrollHeight, 200) + "px";
    syncAtBrowse();
  });
  // 📎 button — opens the multi-select attachment picker.
  $("#chat-attach")?.addEventListener("click", () => {
    if (attachBrowser && attachBrowser.mode === "button") { closeAttachBrowser(); return; }
    openAttachBrowser("button", $("#chat-attach"));
  });
  // Clicking outside the picker / textarea closes it. A short delay lets
  // clicks inside the panel (items, filter) register first.
  document.addEventListener("click", (e) => {
    // Project modal
    if (e.target.closest("#proj-new")) {
      openProjectModal(null);
      return;
    }

    // Chat Mode Select logic removed
    
    // Config tab: models list actions
    const modelBtn = e.target.closest("#models-list button[data-act]");
    if (modelBtn) {
      const act = modelBtn.dataset.act;
      const model = modelBtn.dataset.model;
      if (act === "primary") configAction("set_primary", model);
      else if (act === "remove") {
        if (confirm("Remove model " + model + "?")) configAction("remove_model", model);
      } else if (act === "disable") {
        const durInput = document.querySelector(`.mc-disable-dur[data-model="${CSS.escape(model)}"]`);
        modelDisable(model, durInput?.value?.trim() || "");
      } else if (act === "enable") {
        modelEnable(model);
      }
      return;
    }
    
    // Config tab: provider grid click
    const pCard = e.target.closest(".provider-card");
    if (pCard) {
      const pid = pCard.dataset.pid;
      const sel = $("#cfg-provider");
      if (sel) {
        sel.value = pid;
        sel.dispatchEvent(new Event("change", { bubbles: true }));
        // Show the config modal
        const mod = $("#provider-modal");
        if (mod) mod.style.display = "flex";
      }
      return;
    }
    // Projects Tab modals buttons (Save / Cancel / Browse)
    if (e.target.id === "proj-save") {
      saveProject();
      return;
    }
    if (e.target.id === "proj-cancel") {
      closeProjectModal();
      return;
    }
    if (e.target.id === "proj-browse-btn") {
      openDirPicker("pf-path");
      return;
    }
    
    // Fetch models action
    if (e.target.id === "cfg-fetch-models-btn") {
      fetchProviderModels();
      return;
    }
    
    // Register model action
    if (e.target.id === "cfg-add-btn") {
      (async () => {
        const btn = e.target;
        btn.textContent = "Registering...";
        try {
          const provider = $("#cfg-provider").value;
          const modelName = $("#cfg-model-name").value;
          const apiKey = $("#cfg-api-key").value;
          const baseURL = $("#cfg-base-url").value;
          const res = await fetch(API + "/api/config", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ action: "add_model", provider, model_name: modelName, api_key: apiKey, base_url: baseURL })
          });
          if (!res.ok) throw new Error(await res.text());
          toast("success", `✓ Model ${modelName} registered and hot-reloaded`);
          btn.textContent = "Added!";
          const keyInput = $("#cfg-api-key");
          if (keyInput) keyInput.value = "";
          setTimeout(() => { 
            btn.textContent = "⚡ Register Model";
            const mod = $("#provider-modal");
            if (mod) mod.style.display = "none";
          }, 1000);
          await loadConfig();
        } catch (err) {
          toast("error", "Error: " + err.message);
          btn.textContent = "⚡ Register Model";
        }
      })();
      return;
    }
    
    // Directory Picker actions
    if (e.target.id === "dir-picker-cancel") {
      closeDirPicker();
      return;
    }
    if (e.target.id === "dir-picker-select") {
      if (dirPickerTargetInput && dirPickerCurrentPath) {
        $(dirPickerTargetInput).value = dirPickerCurrentPath;
      }
      closeDirPicker();
      return;
    }
    if (e.target.id === "dir-picker-new-btn") {
      $("#dir-picker-new-row").style.display = "block";
      $("#dir-picker-new-input")?.focus();
      return;
    }
    if (e.target.id === "dir-picker-cancel-new-btn") {
      $("#dir-picker-new-row").style.display = "none";
      $("#dir-picker-new-input").value = "";
      return;
    }
    if (e.target.id === "dir-picker-create-btn") {
      createNewDir();
      return;
    }
    
    // Breadcrumb clicks in dir picker
    const bc = e.target.closest(".breadcrumb");
    if (bc && bc.dataset.path) {
      loadDirPickerContents(bc.dataset.path);
      return;
    }
    
    // Directory list item clicks
    const dItem = e.target.closest(".dir-picker-item");
    if (dItem && dItem.dataset.path) {
      loadDirPickerContents(dItem.dataset.path);
      return;
    }
    
    // Attach browser
    if (!attachBrowser) return;
    const panel = $("#attach-browser");
    const ta = $("#chat-text");
    const btn = $("#chat-attach");
    if (panel && panel.contains(e.target)) return;
    if (ta && ta.contains(e.target)) return;
    if (btn && btn.contains(e.target)) return;
    closeAttachBrowser();
  });
  // NOTE: attach to `window` directly — `$` is document.querySelector, which
  // throws SyntaxError when given a non-string (window → "[object Window]").
  // That throw previously aborted all of attachEventListeners() (so the loop
  // button, model switcher, and command palette handlers were never attached)
  // and then aborted init() before connectSSE()/loadConfig() could run,
  // leaving the model blank and the connection stuck on "Connecting…".
  window.addEventListener("resize", () => { if (attachBrowser) positionAttachBrowser(); });
  
  // Global change listener for dynamic selects
  document.addEventListener("change", (e) => {
    if (e.target.id === "cfg-provider") {
      onProviderChange();
    } else if (e.target.id === "cfg-model-name") {
      onModelChange();
    }
  });

  // Live detail updates as the user types a custom model id (the model field
  // is now a combobox input; "change" only fires on blur, so hook "input" too).
  document.addEventListener("input", (e) => {
    if (e.target.id === "cfg-model-name") {
      onModelChange();
    }
  });

  // Global submit listener for dynamic forms
  document.addEventListener("submit", async (e) => {
    if (e.target.id === "config-settings-form") {
      e.preventDefault();
      const btn = e.target.querySelector("button[type='submit']");
      if (btn) btn.textContent = "Saving...";
      try {
        const routing = $("#cfg-routing").value;
        const safety = $("#cfg-safety").value;
        const turns = parseInt($("#cfg-max-turns").value, 10);
        const res = await fetch(API + "/api/config", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ action: "update_settings", routing_mode: routing, safety_level: safety, max_turns: turns })
        });
        if (!res.ok) throw new Error(await res.text());
        toast("success", "✓ Global settings updated");
        // Re-fetch status so the header routing/safety badge pills update
        // immediately. The backend already hot-reloaded the router mode via
        // ReloadModels; the frontend just wasn't refreshing the badges until
        // page reload. Best-effort: never blocks the toast.
        fetch(API + "/api/status").then(r => r.ok ? r.json() : null).then(d => {
          if (d) { updateBadges(d); updateLoopIndicator(d.agentic_loop); }
        }).catch(() => {});
        if (btn) btn.textContent = "Saved!";
        if (btn) setTimeout(() => { btn.textContent = "Update Settings"; }, 2000);
      } catch (err) {
        toast("error", "Error: " + err.message);
        if (btn) btn.textContent = "Update Settings";
      }
    }
  });

  // Delegated click for attachment chip removal.
  $("#attach-tray")?.addEventListener("click", (e) => {
    const x = e.target.closest(".ac-x");
    if (!x) return;
    const idx = parseInt(x.dataset.idx, 10);
    if (!Number.isNaN(idx)) removeAttachmentAt(idx);
  });

  // File explorer controls
  $("#fe-refresh")?.addEventListener("click", () => { loadFileTree(); toast("info", "Workspace refreshed"); });
  // Manual workspace switcher — prompts for a path and switches the chat
  // console's file explorer to it (independent of project activation).
  $("#fe-switch-ws")?.addEventListener("click", switchWorkspace);
  $("#fe-collapse")?.addEventListener("click", () => {
    if (collapsedDirs.size === 0) {
      // collapse all dirs that have children
      fileTreeData.filter(e => e.is_dir).forEach(e => collapsedDirs.add(e.path));
    } else {
      collapsedDirs.clear();
    }
    fileTreeHash = "";
    renderFileTree();
  });
  $("#fe-mods-clear")?.addEventListener("click", () => {
    fileModLog = [];
    modifiedPaths = {};
    fileTreeHash = "";
    renderModLog();
    renderFileTree();
  });
  $("#fpm-close")?.addEventListener("click", closeFilePreview);
  $("#fpm-copy")?.addEventListener("click", () => {
    const body = $("#fpm-body"); if (!body) return;
    navigator.clipboard?.writeText(body.textContent).then(() => toast("success", "Copied to clipboard"));
  });
  $("#file-preview-modal")?.addEventListener("click", (e) => { if (e.target.id === "file-preview-modal") closeFilePreview(); });

  // Permission popup buttons — each POSTs the decision to the server, which
  // unblocks the waiting tool-dispatch goroutine.
  $("#perm-allow-once")?.addEventListener("click", () => submitApproval("allow-once"));
  $("#perm-allow-session")?.addEventListener("click", () => submitApproval("allow-session"));
  $("#perm-deny")?.addEventListener("click", () => submitApproval("deny"));

  $("#tools-refresh")?.addEventListener("click", loadTools);
  // Tool sources — add/connect/disconnect/remove handlers.
  $("#ts-type")?.addEventListener("change", onToolSourceTypeChange);
  $("#tools-source-form")?.addEventListener("submit", submitToolSource);
  // Show the default field set for the initially-selected source type.
  onToolSourceTypeChange();
  $("#mem-refresh")?.addEventListener("click", loadMemory);
  $("#kg-refresh")?.addEventListener("click", loadKnowledgeGraph);
  // Blueprint tab refresh — re-fetch the active project's plan + workflow.
  $("#blueprint-refresh")?.addEventListener("click", () => {
    if (activeProjectId) {
      fetchProjectPlanAndWorkflow(activeProjectId);
      toast("info", "Blueprint refreshed");
    } else {
      toast("info", "Activate a project to view its plan & workflow");
    }
  });
  // Blueprint task board — regenerate plan/workflow via the LLM rewriter.
  $("#plan-regenerate")?.addEventListener("click", () => regeneratePlanOrWorkflow("plan"));
  $("#workflow-regenerate")?.addEventListener("click", () => regeneratePlanOrWorkflow("workflow"));

  // Projects tab — list, create/edit modal, context editor modal.
  $("#proj-refresh")?.addEventListener("click", () => { loadProjects(); toast("info", "Projects refreshed"); });
  // Event delegation handles #proj-new, #proj-save, #proj-cancel, etc.
  $("#proj-modal")?.addEventListener("click", (e) => { if (e.target.id === "proj-modal") closeProjectModal(); });
  $("#pf-name")?.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); saveProject(); } });
  
  // Directory Picker Modals
  $("#dir-picker-modal")?.addEventListener("click", (e) => { if (e.target.id === "dir-picker-modal") closeDirPicker(); });
  $("#dir-picker-new-input")?.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); createNewDir(); } });
  $("#ctx-close")?.addEventListener("click", closeContextEditor);
  $("#ctx-save")?.addEventListener("click", () => saveContext(false));
  $("#ctx-set-active")?.addEventListener("click", () => saveContext(true));
  $("#ctx-rewrite")?.addEventListener("click", rewriteContext);
  $("#ctx-modal")?.addEventListener("click", (e) => { if (e.target.id === "ctx-modal") closeContextEditor(); });
  $("#ctx-body")?.addEventListener("keydown", (e) => {
    // Ctrl/Cmd+Enter saves the context body.
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); saveContext(false); }
  });

  // Chat-tab active-project banner: view context, switch project, deactivate.
  // ── Project dropdown (toolbar) ── Replaces the old chat-project-bar's
  // inline View Context / Change / Deactivate buttons with a compact dropdown.
  const ctProjBtn = $("#ct-project-btn");
  const ctProjDropdown = $("#ct-project-dropdown");
  ctProjBtn?.addEventListener("click", (e) => {
    e.stopPropagation();
    if (ctProjDropdown) ctProjDropdown.hidden = !ctProjDropdown.hidden;
  });
  document.addEventListener("click", () => { if (ctProjDropdown) ctProjDropdown.hidden = true; });
  ctProjDropdown?.addEventListener("click", (e) => e.stopPropagation());

  $("#ct-view")?.addEventListener("click", () => {
    if (ctProjDropdown) ctProjDropdown.hidden = true;
    if (activeProjectId) openContextEditor(activeProjectId);
    else switchTab("projects");
  });
  $("#ct-change")?.addEventListener("click", () => {
    if (ctProjDropdown) ctProjDropdown.hidden = true;
    switchTab("projects");
  });
  $("#ct-new-project")?.addEventListener("click", () => {
    if (ctProjDropdown) ctProjDropdown.hidden = true;
    openProjectModal(null);
  });
  $("#ct-deactivate")?.addEventListener("click", async () => {
    if (ctProjDropdown) ctProjDropdown.hidden = true;
    if (!activeProjectId) { toast("info", "No active project to deactivate."); return; }
    await setActiveProject(null);
    try {
      const wsRes = await fetch(API + "/api/workspace");
      if (wsRes.ok) {
        const ws = await wsRes.json();
        toast("info", `Project deactivated — workspace reverted to: ${ws.path}`);
      } else {
        toast("info", "Project deactivated — context no longer injected.");
      }
    } catch {
      toast("info", "Project deactivated — context no longer injected.");
    }
    if (activeTab === "projects") loadProjects();
  });
  $("#learn-refresh")?.addEventListener("click", loadLearning);
  $("#audit-refresh")?.addEventListener("click", loadAudit);
  $("#status-refresh")?.addEventListener("click", loadStatus);
  $("#res-refresh")?.addEventListener("click", loadStatus);
  $("#config-refresh")?.addEventListener("click", loadConfig);

  $("#mon-refresh")?.addEventListener("click", () => { loadMetrics(); toast("info", "Metrics refreshed"); });
  $("#mon-reset")?.addEventListener("click", async () => {
    if (!confirm("Reset all usage metrics? This cannot be undone.")) return;
    try {
      await fetch(API + "/api/metrics/reset", { method: "POST" });
      loadMetrics();
      toast("success", "Metrics reset");
    } catch (err) { toast("error", "Error: " + err.message); }
  });

  // Settings form
  $("#config-settings-form")?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector("button");
    btn.textContent = "Updating...";
    try {
      const res = await fetch(API + "/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          action: "update_settings",
          routing_mode: $("#cfg-routing").value,
          safety_level: $("#cfg-safety").value,
          max_turns: parseInt($("#cfg-max-turns").value, 10)
        })
      });
      if (!res.ok) throw new Error(await res.text());
      toast("success", "Settings updated");
      btn.textContent = "Saved!";
      setTimeout(() => { btn.textContent = "Update Settings"; }, 2000);
    } catch (err) {
      toast("error", "Error: " + err.message);
      btn.textContent = "Update Settings";
    }
  });

  // Add model form (provider-driven)
  $("#cfg-provider")?.addEventListener("change", onProviderChange);
  $("#cfg-model-name")?.addEventListener("change", onModelChange);
  $("#cfg-api-key")?.addEventListener("change", () => {
    const id = $("#cfg-provider").value;
    const p = providerCatalog.find(x => x.id === id);
    if (p && !p.local) {
      fetchProviderModels();
    }
  });

  // Agentic Loop toggle — flip persists immediately to the backend (so the
  // loop can be enabled/disabled in one action and survives a reload), and
  // the label + header indicator update live. The Apply button below remains
  // for updating max-loops.
  $("#cfg-agentic-loop")?.addEventListener("change", async (e) => {
    const on = e.target.checked;
    const lbl = $("#cfg-agentic-loop-label");
    if (lbl) lbl.textContent = on ? "Enabled" : "Disabled";
    const loops = parseInt($("#cfg-max-loops")?.value, 10) || 20;
    try {
      const res = await fetch(API + "/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "update_settings", agentic_loop: on, max_loops: loops })
      });
      if (!res.ok) throw new Error(await res.text());
      updateLoopIndicator(on);
      updateLoopModeOption(on);
      toast("success", on ? `Agentic Loop enabled (max ${loops} iterations)` : "Agentic Loop disabled");
    } catch (err) {
      // Revert the checkbox to the last-known server state.
      toast("error", "Loop toggle failed: " + err.message);
      loadConfig();
    }
  });

  // Agentic Loop Apply button — persist enable + max-loops to the backend
  // and refresh the header indicator.
  $("#cfg-loop-btn")?.addEventListener("click", async () => {
    const btn = $("#cfg-loop-btn");
    const on = $("#cfg-agentic-loop").checked;
    const loops = parseInt($("#cfg-max-loops").value, 10) || 20;
    const prev = btn.textContent;
    btn.textContent = "Applying...";
    try {
      const res = await fetch(API + "/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "update_settings", agentic_loop: on, max_loops: loops })
      });
      if (!res.ok) throw new Error(await res.text());
      updateLoopIndicator(on);
      updateLoopModeOption(on);
      toast("success", on ? `Agentic Loop enabled (max ${loops} iterations)` : "Agentic Loop disabled");
      btn.textContent = "Saved!";
      setTimeout(() => { btn.textContent = prev; }, 2000);
    } catch (err) {
      toast("error", "Error: " + err.message);
      btn.textContent = prev;
    }
  });

  $("#cfg-enable-local-llm")?.addEventListener("change", async (e) => {
    const lbl = $("#cfg-enable-local-llm-label");
    if (lbl) {
      lbl.textContent = e.target.checked ? "Auto-load ON" : "Auto-load OFF";
      lbl.style.color = e.target.checked ? "var(--text-bright)" : "var(--text-mute)";
    }
  });

  $("#cfg-enable-local-offload")?.addEventListener("change", async (e) => {
    const lbl = $("#cfg-enable-local-offload-label");
    if (lbl) {
      lbl.textContent = e.target.checked ? "Offloading ON" : "Offloading OFF";
      lbl.style.color = e.target.checked ? "var(--text-bright)" : "var(--text-mute)";
    }
  });

  $("#cfg-local-llm-btn")?.addEventListener("click", async () => {
    const on = $("#cfg-enable-local-llm").checked;
    const offload = $("#cfg-enable-local-offload").checked;
    const force = $("#cfg-force-local")?.checked || false;
    const memoryProfile = $("#cfg-memory-profile")?.value ?? "";
    // Force implies local enabled. local_mode drives routing: "force" pins to
    // local (no cloud fallback), "auto" is the gated default, "off" disables.
    const localMode = force ? "force" : (on ? "auto" : "off");
    const btn = $("#cfg-local-llm-btn");
    btn.innerHTML = '<div class="typing-dot" style="margin:4px auto"></div>';
    try {
      const res = await fetch(API + "/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "update_settings", enable_local_llm: on || force, local_mode: localMode, enable_local_offloading: offload, memory_profile: memoryProfile }),
      });
      if (!res.ok) throw new Error("save failed");
      const data = await res.json().catch(() => ({}));
      btn.innerHTML = "Saved!";
      setTimeout(() => (btn.innerHTML = "Apply"), 2000);
      // A force-local start failure comes back as a warning — surface it
      // rather than pretending it succeeded (never a silent cloud fallback).
      if (data.warning) {
        toast("error", data.warning);
      } else if (data.force_local) {
        toast("success", "Force Local active — every request now uses the local model.");
      } else {
        toast("success", "Local LLM setting saved." + (on ? " Model loads on next restart." : ""));
      }
      if (typeof renderForceLocalBadge === "function") renderForceLocalBadge(!!data.force_local);
    } catch (err) {
      console.error(err);
      btn.innerHTML = "Error";
      setTimeout(() => (btn.innerHTML = "Apply"), 2000);
      toast("error", "Error: " + err.message);
    }
  });

  // Execution Profile segment toggle — immediate-apply (mirrors the loop
  // toggle). Switches the parallelism profile (Auto/Sequential/Parallel)
  // and updates the active segment + hint without a full reload.
  $$(".cfg-seg-btn").forEach(btn => {
    btn?.addEventListener("click", async () => {
      const profile = btn.dataset.profile;
      try {
        const res = await fetch(API + "/api/config", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ action: "update_settings", execution_profile: profile })
        });
        if (!res.ok) throw new Error(await res.text());
        renderExecutionProfile(profile);
        toast("success", `Execution profile set to ${profile}`);
      } catch (err) {
        toast("error", "Profile switch failed: " + err.message);
        loadConfig();
      }
    });
  });

  // Model switcher (topbar)
  $("#model-select")?.addEventListener("change", (e) => {
    const model = e.target.value;
    if (model) configAction("set_primary", model);
  });

  // Reopen the pending-approval popup (F4: previously an inline
  // onclick="reopenApprovalModal()" attribute on the nexus.html button).
  $("#reopen-approval-btn")?.addEventListener("click", () => {
    if (typeof reopenApprovalModal === "function") reopenApprovalModal();
  });

  // Consensus role dropdown (per-model, in config tab). Uses event delegation
  // on #models-list because cards are dynamically re-rendered by loadConfig.
  $("#models-list")?.addEventListener("change", (e) => {
    const sel = e.target.closest('select[data-act="role"]');
    if (!sel) return;
    const model = sel.dataset.model;
    const role = sel.value;
    configActionRole(model, role);
  });

  // Compressor model dropdown (Global Settings). Sends immediately on change
  // so the compressor is hot-reloaded without needing to click Update Settings.
  $("#cfg-compressor-model")?.addEventListener("change", async (e) => {
    const model = e.target.value;
    try {
      const res = await fetch(API + "/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "set_compressor", compressor_model: model })
      });
      if (!res.ok) throw new Error(await res.text());
      toast("success", model ? `✓ Compressor set to ${model}` : "✓ Compressor reset to primary model");
    } catch (err) {
      toast("error", "Error: " + err.message);
    }
  });

  // Command palette
  $("#cmdk-hint")?.addEventListener("click", openCmdPalette);
  $("#cmd-input")?.addEventListener("input", (e) => renderCmdResults(e.target.value));
  $("#cmd-input")?.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); moveCmdSelection(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); moveCmdSelection(-1); }
    else if (e.key === "Enter") { e.preventDefault(); execCmdSelection(); }
    else if (e.key === "Escape") { closeCmdPalette(); }
  });
  $("#cmd-palette")?.addEventListener("click", (e) => { if (e.target.id === "cmd-palette") closeCmdPalette(); });

  // Global keyboard shortcuts
  document.addEventListener("keydown", (e) => {
    // Cmd/Ctrl + K → command palette
    if ((e.metaKey || e.ctrlKey) && e.key === "k") {
      e.preventDefault();
      const p = $("#cmd-palette");
      if (p && p.classList.contains("open")) closeCmdPalette();
      else openCmdPalette();
      return;
    }
    // Escape → close palette / modals
    if (e.key === "Escape") {
      if (attachBrowser) { closeAttachBrowser(); return; }
      const modal = $("#file-preview-modal");
      if (modal && !modal.hidden) { closeFilePreview(); return; }
      if ($("#proj-modal") && $("#proj-modal").classList.contains("active")) { closeProjectModal(); return; }
      if ($("#ctx-modal") && $("#ctx-modal").classList.contains("active")) { closeContextEditor(); return; }
      closeCmdPalette(); return;
    }
    // Number keys 1-9 + 0 → switch tabs (when not typing in an input)
    if (e.key >= "0" && e.key <= "9" && !["INPUT","TEXTAREA","SELECT"].includes(document.activeElement.tagName)) {
      const tabs = ["nexus","blueprint","registry","telemetry","config"];
      const idx = e.key === "0" ? 9 : parseInt(e.key, 10) - 1;
      if (tabs[idx]) switchTab(tabs[idx]);
    }
  });
}

// ════════════════════════════════════════════════════════════════════════
