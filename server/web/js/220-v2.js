/* 220-v2.js — extracted from app.js (lines 4526-4991) */
// V2 EXTENSION MODULE — Execution Pipeline,
//   Consensus, Verification, Resource Monitor, Intelligence
// ════════════════════════════════════════════════════════════════════════
(function initV2Extensions() {
  "use strict";

  // ── Execution Status Bar ───────────────────────────────────────────
  const EXEC_STAGES = ["planning","retrieval","compression","tools","execution","verification","reflection","completion"];
  let execActive = false;
  let execTimer = null;
  let execStartTime = 0;
  let execTokenCount = 0;
  // Per-response execution trace (Fix D). Captured during the run and folded
  // into a collapsible toggle under the assistant message on completion —
  // replaces the removed global "View/Hide Execution Details" toggle.
  let currentExecTrace = [];

  function showExecBar() {
    const bar = document.getElementById("exec-status-bar");
    if (bar) bar.hidden = false;

    // Reset the per-response execution trace (Fix D). The global
    // "View/Hide Execution Details" toggle was removed; each response now
    // carries its own collapsible trace attached on completion.
    currentExecTrace = [];

    execActive = true;
    execStartTime = Date.now();
    execTokenCount = 0;
    resetExecStages();
    startExecTimer();
  }

  function hideExecBar() {
    execActive = false;
    if (execTimer) { clearInterval(execTimer); execTimer = null; }
  }

  function collapseExecTimeline() {
    execActive = false;
    if (execTimer) { clearInterval(execTimer); execTimer = null; }
    // Hide the live pipeline bar once the run finishes. The per-response
    // collapsible trace (attachExecDetails) carries the detail onward.
    const bar = document.getElementById("exec-status-bar");
    if (bar) bar.hidden = true;
  }

  function resetExecStages() {
    EXEC_STAGES.forEach(s => {
      const el = document.querySelector(`.exec-stage[data-stage="${s}"]`);
      if (el) { el.className = "exec-stage"; }
    });
  }

  function setExecStage(stage, state) {
    const el = document.querySelector(`.exec-stage[data-stage="${stage}"]`);
    if (el) { el.className = `exec-stage ${state}`; }
  }

  function startExecTimer() {
    if (execTimer) clearInterval(execTimer);
    execTimer = setInterval(() => {
      const elapsed = (Date.now() - execStartTime) / 1000;
      const durEl = document.getElementById("exec-duration");
      if (durEl) durEl.textContent = elapsed < 60 ? elapsed.toFixed(1) + "s" : (elapsed / 60).toFixed(1) + "m";
    }, 100);
  }

  function updateExecMetric(id, value) {
    const el = document.getElementById(id);
    if (el) el.textContent = value;
  }

  // ── Per-Message Metadata ───────────────────────────────────────────
  let lastMsgMeta = {};

  function renderMsgMeta(msgEl) {
    if (!msgEl || !lastMsgMeta.model) return;
    // Only add meta to assistant messages
    if (!msgEl.classList.contains("msg-assistant")) return;
    // Don't add twice
    if (msgEl.querySelector(".msg-meta-row")) return;

    const meta = document.createElement("div");
    meta.className = "msg-meta-row";
    const items = [
      { label: "model", value: lastMsgMeta.model || "—" },
      { label: "provider", value: lastMsgMeta.provider || "—" },
      { label: "tokens", value: lastMsgMeta.tokens || "—" },
      { label: "cost", value: lastMsgMeta.cost || "—" },
      { label: "latency", value: lastMsgMeta.latency || "—" },
    ];
    meta.innerHTML = items.map(i =>
      `<span class="msg-meta-item"><span class="meta-label">${i.label}</span> <span class="meta-value">${i.value}</span></span>`
    ).join("");
    msgEl.appendChild(meta);
  }

  // attachExecDetails folds the per-response execution trace (captured
  // during the run) into a collapsible toggle just below the assistant
  // message. Collapsed by default; click the chevron to expand. Replaces
  // the removed global "View/Hide Execution Details" toggle (Fix D).
  function attachExecDetails(msgEl) {
    if (!msgEl || msgEl.querySelector(".msg-exec-details")) return;
    if (!currentExecTrace || currentExecTrace.length === 0) return;
    const wrap = document.createElement("div");
    wrap.className = "msg-exec-details";
    const toggle = document.createElement("div");
    toggle.className = "msg-exec-toggle";
    toggle.innerHTML = '<span class="msg-exec-chevron">▶</span> Execution Details <span class="msg-exec-count">(' + currentExecTrace.length + ')</span>';
    const body = document.createElement("div");
    body.className = "msg-exec-body";
    body.style.display = "none";
    body.innerHTML = currentExecTrace.join("");
    toggle.addEventListener("click", () => {
      const open = body.style.display !== "none";
      body.style.display = open ? "none" : "block";
      const ch = toggle.querySelector(".msg-exec-chevron");
      if (ch) ch.textContent = open ? "▶" : "▼";
    });
    wrap.appendChild(toggle);
    wrap.appendChild(body);
    msgEl.appendChild(wrap);
  }

  // ── Consensus State ────────────────────────────────────────────────
  let consensusHistory = [];

  function handleConsensusEvent(evt) {
    let contentStr = "";
    let conflict = evt.status === "conflict";
    let modelCount = 0;
    
    if (typeof evt.content === "object" && evt.content !== null) {
      conflict = evt.content.conflict || conflict;
      modelCount = evt.content.model_count || 0;
      contentStr = evt.content.message || JSON.stringify(evt.content);
      updateExecMetric("cs-model-count", String(modelCount));
    } else {
      contentStr = String(evt.content);
    }

    // Update KPIs
    updateExecMetric("cs-conflict", conflict ? "⚠️ CONFLICT DETECTED" : "✓ no conflict");
    const liveEl = document.getElementById("consensus-live");
    if (liveEl) { liveEl.textContent = "● ACTIVE"; liveEl.style.color = conflict ? "var(--red)" : "var(--green)"; }

    // Add to history
    consensusHistory.push({ time: new Date().toLocaleTimeString(), content: contentStr, conflict, timestamp: evt.timestamp });
    renderConsensusHistory();

  }

  function renderConsensusHistory() {
    const el = document.getElementById("cs-history");
    if (!el) return;
    if (consensusHistory.length === 0) {
      el.innerHTML = '<div class="mem-empty">No consensus history yet.</div>';
      return;
    }
    el.innerHTML = consensusHistory.slice(-20).reverse().map(h => `
      <div class="consensus-history-item" style="border-left: 3px solid ${h.conflict ? 'var(--red)' : 'var(--green)'};">
        <div style="display:flex; justify-content:space-between; margin-bottom:4px;">
          <span style="font-weight:700; color:var(--text-bright);">${h.conflict ? '⚠️ Conflict' : '✓ Consensus'}</span>
          <span style="color:var(--text-mute); font-size:10px;">${h.time}</span>
        </div>
        <div style="font-family:var(--font-mono); font-size:11px; color:var(--text-dim);">${h.content.substring(0, 200)}${h.content.length > 200 ? '…' : ''}</div>
      </div>
    `).join("");
  }

  // ── Verification Pipeline State ────────────────────────────────────
  const VERIFY_STAGES = ["formatter","compiler","tests","lint","security","performance","complexity","coverage","patch"];
  let verificationHistory = [];

  function updateVerifyStage(stage, state, duration) {
    const stageEl = document.querySelector(`.verify-stage[data-stage="${stage}"]`);
    if (stageEl) {
      stageEl.setAttribute("data-state", state);
      const statusEl = stageEl.querySelector(".verify-stage-status");
      if (statusEl) statusEl.textContent = state;
      const durEl = stageEl.querySelector(".verify-stage-duration");
      if (durEl && duration) durEl.textContent = duration;
    }
  }

  function resetVerifyPipeline() {
    VERIFY_STAGES.forEach(s => updateVerifyStage(s, "pending", "—"));
  }

  // ── Resource Monitor ───────────────────────────────────────────────
  let resourcePollTimer = null;

  async function pollResources() {
    try {
      const res = await fetch("/api/system/resources");
      if (!res.ok) return;
      const data = await res.json();
      renderResourceTiles(data);
    } catch { /* endpoint may not exist yet */ }
  }

  function renderResourceTiles(data) {
    // Update existing resource tiles if they exist
    const grid = document.getElementById("resource-grid");
    if (!grid || !data) return;

    const tiles = [
      { label: "CPU", value: (data.cpu_percent || 0).toFixed(1) + "%", pct: data.cpu_percent || 0 },
      { label: "Memory", value: formatBytes(data.mem_used || 0), pct: data.mem_percent || 0, sub: formatBytes(data.mem_total || 0) + " total" },
      { label: "Goroutines", value: String(data.goroutines || 0) },
      { label: "GC Cycles", value: String(data.gc_cycles || 0) },
      { label: "Heap Alloc", value: formatBytes(data.heap_alloc || 0) },
      { label: "Stack", value: formatBytes(data.stack_inuse || 0) },
    ];

    grid.innerHTML = tiles.map(t => `
      <div class="resource-tile">
        <div class="resource-label">${t.label}</div>
        <div class="resource-value">${t.value}</div>
        ${t.pct !== undefined ? `<div class="resource-bar"><div class="resource-bar-fill ${t.pct > 80 ? 'high' : t.pct > 50 ? 'medium' : 'low'}" style="width:${Math.min(t.pct, 100)}%"></div></div>` : ''}
        ${t.sub ? `<div class="resource-sub">${t.sub}</div>` : ''}
      </div>
    `).join("");
  }

  function formatBytes(b) {
    if (b < 1024) return b + " B";
    if (b < 1048576) return (b / 1024).toFixed(1) + " KB";
    if (b < 1073741824) return (b / 1048576).toFixed(1) + " MB";
    return (b / 1073741824).toFixed(2) + " GB";
  }

  function startResourcePoll() {
    if (resourcePollTimer) return;
    pollResources();
    // (P4) Skip the fetch while the document is hidden — the resource tiles
    // are only on the monitoring tab and aren't visible, so the poll is
    // wasted work + a constant goroutine on the backend.
    resourcePollTimer = setInterval(() => { if (!document.hidden) pollResources(); }, 3000);
  }

  function stopResourcePoll() {
    if (resourcePollTimer) { clearInterval(resourcePollTimer); resourcePollTimer = null; }
  }

  // ── Enhanced SSE Event Router ──────────────────────────────────────
  // Extend the existing SSE handler to process V2 event types.
  // We hook into the global event system by wrapping the existing addEvent.
  const _origAddEvent = window.addEvent;
  if (typeof _origAddEvent === "function") {
    window.addEvent = function(evt) {
      // Call original handler first
      _origAddEvent(evt);
      // V2 event routing
      handleV2Event(evt);
    };
  }

  function handleV2Event(evt) {
    if (!evt || !evt.type) return;

    switch (evt.type) {
      case "task_update":
        handleTaskUpdate(evt);
        break;
      case "model_route":
        if (evt.content) {
          const content = String(evt.content);
          const modelMatch = content.match(/model=(\S+)/);
          if (modelMatch) updateExecMetric("exec-model", modelMatch[1]);
          updateExecMetric("exec-provider", evt.agent || "—");
        }
        break;
      case "compression":
        if (evt.content) {
          updateExecMetric("exec-compress-ratio", String(evt.content));
          setExecStage("compression", "completed");
        }
        break;
      case "consensus":
        handleConsensusEvent(evt);
        break;
      case "token_usage":
        handleTokenUsage(evt);
        break;
      case "tool_execution":
        if (evt.status === "executing" || evt.status === "started") {
          setExecStage("tools", "running");
        } else {
          setExecStage("tools", "completed");
        }
        
        // Show tool execution in the trace so the user isn't blind
        if (evt.tool) {
          const timeStr = new Date(evt.timestamp || Date.now()).toLocaleTimeString();
          let msg = "";
          if (evt.status === "completed" && typeof evt.content === "object" && evt.content) {
             msg = evt.content.error ? "failed" : "completed";
          } else {
             msg = evt.status || "executed";
          }
          const div = document.createElement("div");
          div.style.marginBottom = "4px";
          div.innerHTML = `<span style="color:var(--text-mute)">[${timeStr}]</span> <span style="color:var(--accent-1)">[tool]</span> ${evt.tool} - ${msg}`;
          currentExecTrace.push(div.outerHTML);
          
          const loadingMsg = document.querySelector(".msg.loading");
          if (loadingMsg) {
            const timeline = loadingMsg.querySelector(".inline-exec-timeline");
            if (timeline) {
              timeline.hidden = false;
              timeline.appendChild(div.cloneNode(true));
              timeline.scrollTop = timeline.scrollHeight;
            }
          }
        }
        break;
      case "chat_query":
        showExecBar();
        setExecStage("planning", "running");
        break;
      case "chat_response":
      case "final_output":
        setExecStage("completion", "completed");
        // Safety-net finalize: if the SSE final_output arrives before the
        // fetch resolves (or the fetch errored), finalize the loading message
        // now so the live trace is preserved. Idempotent — the fetch path's
        // own finalizeAssistantMessage call will no-op if this already ran.
        if (window.DC && typeof window.DC.finalizeAssistantMessage === "function") {
          setTimeout(() => {
            const loadingMsg = document.querySelector(".msg.loading");
            if (loadingMsg) {
              const output = evt.content || "(empty response)";
              window.DC.finalizeAssistantMessage(loadingMsg, output, false, false);
            }
          }, 50);
        }
        // Add message metadata + the per-response execution trace to the
        // last assistant message (Fix D). Collapsed by default behind a
        // chevron toggle — replaces the removed global "Hide Execution
        // Details" bar. Only runs for non-loading (already finalized or
        // history-replayed) messages; the loading case is handled above.
        setTimeout(() => {
          const msgs = document.querySelectorAll(".msg.assistant");
          if (msgs.length > 0) {
            const last = msgs[msgs.length - 1];
            renderMsgMeta(last);
            // attachExecDetails is the fallback for history-replayed messages
            // that had no live timeline. For freshly finalized messages the
            // trace is already folded in by finalizeAssistantMessage.
            if (!last.querySelector(".msg-exec-details") && !last.querySelector(".inline-exec-timeline")) {
              attachExecDetails(last);
            }
          }
        }, 120);
        setTimeout(collapseExecTimeline, 500);
        break;
      case "error":
        if (execActive) {
          EXEC_STAGES.forEach(s => {
            const el = document.querySelector(`.exec-stage[data-stage="${s}"]`);
            if (el && el.classList.contains("running")) setExecStage(s, "failed");
          });
          setTimeout(collapseExecTimeline, 5000);
        }
        break;
    }
  }

  function handleTaskUpdate(evt) {
    const content = String(evt.content || "").toLowerCase();
    const status = String(evt.status || "").toLowerCase();
    
    // Streaming token chunks are NOT execution detail — they are the live
    // LLM output (removed from the execution-detail per the request).
    // Skip them so the per-response trace stays a readable orchestration
    // log (plan / route / compress / tools / verify), not a token spam.
    if (status === "streaming") return;

    // Append to timeline
    const timeStr = new Date(evt.timestamp || Date.now()).toLocaleTimeString();
    const div = document.createElement("div");
    div.style.marginBottom = "4px";
    div.innerHTML = `<span style="color:var(--text-mute)">[${timeStr}]</span> <span style="color:var(--accent-3)">[${evt.status || 'info'}]</span> ${evt.content}`;
    
    // Capture this event into the per-response trace (Fix D). The global
    // timeline was removed; the trace is attached to the response on
    // completion via attachExecDetails.
    if (evt.content) {
      currentExecTrace.push(div.outerHTML);
    }

    // LIVE execution detail: append the row to the current loading message's
    // .inline-exec-timeline so the user sees what the model is doing in real
    // time (ChatGPT-style). The timeline is converted to a collapsible toggle
    // on completion by finalizeAssistantMessage.
    if (evt.content) {
      const loadingMsg = document.querySelector(".msg.loading");
      if (loadingMsg) {
        const timeline = loadingMsg.querySelector(".inline-exec-timeline");
        if (timeline) {
          timeline.hidden = false;
          timeline.appendChild(div.cloneNode(true));
          timeline.scrollTop = timeline.scrollHeight;
        }
      }
    }

    // Map task updates to pipeline stages
    if (content.includes("plan") || content.includes("classif")) {
      if (status === "completed" || status === "done") { setExecStage("planning", "completed"); setExecStage("retrieval", "running"); }
      else setExecStage("planning", "running");
    }
    if (content.includes("context") || content.includes("retriev") || content.includes("inject")) {
      if (status === "completed" || status === "done") { setExecStage("retrieval", "completed"); setExecStage("execution", "running"); }
      else setExecStage("retrieval", "running");
    }
    if (content.includes("compress") || content.includes("summary")) {
      if (status === "completed" || status === "done") setExecStage("compression", "completed");
      else setExecStage("compression", "running");
    }
    if (content.includes("route") || content.includes("routed")) {
      setExecStage("planning", "completed");
    }
    if (content.includes("verif")) {
      if (status === "completed" || status === "done") setExecStage("verification", "completed");
      else setExecStage("verification", "running");
    }
    if (content.includes("reflect")) {
      if (status === "completed" || status === "done") setExecStage("reflection", "completed");
      else setExecStage("reflection", "running");
    }
    if (content.includes("execut")) {
      if (status === "completed" || status === "done") setExecStage("execution", "completed");
      else setExecStage("execution", "running");
    }
  }

  function handleTokenUsage(evt) {
    if (!evt.content) return;
    const stats = typeof evt.content === "object" ? evt.content : {};
    const prompt = stats.prompt_tokens || 0;
    const completion = stats.completion_tokens || 0;
    const total = prompt + completion;
    const cost = stats.cost || 0;
    const latency = stats.latency_ms || 0;

    execTokenCount += total;

    updateExecMetric("exec-tokens", execTokenCount.toLocaleString());
    updateExecMetric("exec-cost", "$" + cost.toFixed(4));
    updateExecMetric("exec-latency", latency > 0 ? latency + "ms" : "—");
    updateExecMetric("exec-context", prompt > 0 ? prompt.toLocaleString() + " tok" : "—");

    // Store for per-message metadata
    lastMsgMeta = {
      model: stats.model || "—",
      provider: stats.provider || "—",
      tokens: total.toLocaleString(),
      cost: "$" + cost.toFixed(4),
      latency: latency > 0 ? latency + "ms" : "—"
    };
  }

  // ── Tab Switch Hook — start/stop resource polling ──────────────────
  const _origSwitchTab = window.switchTab;
  if (typeof _origSwitchTab === "function") {
    window.switchTab = function(tab) {
      _origSwitchTab(tab);
      // Start resource polling when monitoring tab is active
      // Resource polling now follows the consolidated Telemetry tab
      // (monitoring was merged into telemetry during the nav refactor).
      if (tab === "telemetry") startResourcePoll();
      else stopResourcePoll();
    };
  }

  // ── Intelligence Refresh ───────────────────────────────────────────
  document.getElementById("intel-refresh")?.addEventListener("click", loadIntelligence);

  async function loadIntelligence() {
    try {
      const res = await fetch("/api/intelligence/summary");
      if (!res.ok) return;
      const data = await res.json();
      // Field names match intelligence.ProjectIndex.Stats():
      //   total_symbols, functions, types, packages, indexed_files,
      //   call_edges, class_types, language, lsp_connected, health
      updateExecMetric("intel-files", String(data.indexed_files || 0));
      updateExecMetric("intel-symbols", String(data.total_symbols || 0));
      updateExecMetric("intel-funcs", String(data.functions || 0));
      updateExecMetric("intel-classes", String(data.class_types || data.types || 0));
      updateExecMetric("intel-health", data.health || "—");
      const sub = document.getElementById("intel-files-sub");
      if (sub) sub.textContent = String(data.packages || 0) + " pkgs · " + String(data.call_edges || 0) + " calls";
      // Dependency summary: there is no dedicated dep-graph endpoint, so
      // render the package + call-edge stats (+ language/LSP status) from
      // the SAME summary response so the grid is never stuck on
      // "No dependency data available." No backend change.
      const deps = document.getElementById("intel-deps");
      if (deps) {
        const lang = data.language || "—";
        const lsp = data.lsp_connected ? "LSP ✓" : "AST fallback";
        const pkgs = Number(data.packages || 0);
        const calls = Number(data.call_edges || 0);
        const files = Number(data.indexed_files || 0);
        deps.innerHTML =
          '<div class="kg-item"><span class="kg-badge">lang</span> <strong>' + esc(String(lang)) + '</strong><br><span style="color:var(--text-mute)">' + esc(lsp) + '</span></div>' +
          '<div class="kg-item"><span class="kg-badge">packages</span> <strong>' + pkgs + '</strong><br><span style="color:var(--text-mute)">dependency nodes</span></div>' +
          '<div class="kg-item"><span class="kg-badge">call edges</span> <strong>' + calls + '</strong><br><span style="color:var(--text-mute)">across ' + files + ' files</span></div>';
      }
    } catch { /* endpoint may not exist yet */ }
  }
  // Expose so switchTab() (js/00-core.js) can hydrate the Cognition tab.
  window.loadIntelligence = loadIntelligence;

  // ── Log Category Filtering ────────────────────────────────────────
  // Inject category tabs above the events list when the events tab loads.
  const observer = new MutationObserver(() => {
    const eventsList = document.getElementById("events-list") || document.querySelector(".events-list");
    if (!eventsList || document.querySelector(".log-category-tabs")) return;

    const categories = ["All","Scheduler","Routing","Consensus","Prompt","Compression","Retrieval","Verification","Provider","Resource","Indexing","Developer"];
    const tabsDiv = document.createElement("div");
    tabsDiv.className = "log-category-tabs";
    tabsDiv.innerHTML = categories.map((c, i) =>
      `<button class="log-cat-tab${i === 0 ? ' active' : ''}" data-cat="${c.toLowerCase()}">${c}</button>`
    ).join("");

    tabsDiv.addEventListener("click", (e) => {
      const tab = e.target.closest(".log-cat-tab");
      if (!tab) return;
      tabsDiv.querySelectorAll(".log-cat-tab").forEach(t => t.classList.remove("active"));
      tab.classList.add("active");
      const catMap = {
        "scheduler": ["scheduler", "executor", "planner", "execution", "agent"],
        "routing": ["routing", "router"],
        "consensus": ["consensus"],
        "prompt": ["prompt", "direct", "general"],
        "compression": ["compression", "summary", "compress"],
        "retrieval": ["retrieval", "retriever", "memory", "attachments"],
        "verification": ["verification", "verifier", "verify"],
        "provider": ["provider", "client", "llm"],
        "resource": ["resource", "sandbox", "security"],
        "indexing": ["indexing", "kg", "intel"],
        "developer": ["developer", "agentic-loop", "kernel", "debug"]
      };
      const searchTerms = catMap[cat] || [cat];
      const items = eventsList.querySelectorAll(".evt-item, .evt-group");
      items.forEach(item => {
        if (cat === "all") { item.style.display = ""; return; }
        const type = item.querySelector(".evt-type")?.textContent?.toLowerCase() || "";
        const content = item.querySelector(".evt-content")?.textContent?.toLowerCase() || "";
        const matches = searchTerms.some(term => type.includes(term) || content.includes(term));
        item.style.display = matches ? "" : "none";
      });
    });

    const toolbar = eventsList.previousElementSibling;
    if (toolbar) toolbar.after(tabsDiv);
    else eventsList.parentElement?.insertBefore(tabsDiv, eventsList);
  });

  observer.observe(document.body, { childList: true, subtree: true });

  console.log("[V2] Extension module loaded — developer mode, execution pipeline, consensus, verification, resource monitor");
})();

