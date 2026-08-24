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
  // Live streamed answer text (Stream fix). Every model call the run makes
  // — each ReAct iteration, the general no-tools path, each DAG subagent —
  // emits its own token deltas as task_update/status:"streaming" events
  // tagged with a source id (evt.task_id). streamBuf accumulates the
  // CURRENTLY active source only; it resets whenever a different source
  // starts streaming (a subagent taking over, or a fresh ReAct iteration —
  // see the "thinking" check in handleTaskUpdate below), the same "last
  // write wins, a stray interleave costs nothing" rule the CLI's own
  // streaming preview already uses (surfaces/cli/console.go) — nothing here
  // is authoritative; finalizeAssistantMessage always overwrites it with the
  // real returned answer once the run completes.
  let streamBuf = "";
  let streamSourceID = null;
  let streamGenPending = false;
  let streamFlushScheduled = false;
  // Coalescing state: consecutive identical trace rows (e.g. the same tool
  // called again and again) collapse into one row with a "×N" counter instead
  // of stacking dozens of duplicate lines.
  let lastExecKey = null;
  let lastExecCount = 1;

  // pushExecRow adds a trace row to the live timeline and the folded trace,
  // coalescing a repeat of the immediately-previous row into a ×N counter.
  function pushExecRow(timeline, key, rowLabel) {
    const render = (n) => `<div class="exec-row" style="margin-bottom:4px">${rowLabel}${n > 1 ? ` <span style="color:var(--text-mute)">×${n}</span>` : ""}</div>`;
    if (key === lastExecKey && currentExecTrace.length) {
      lastExecCount++;
      currentExecTrace[currentExecTrace.length - 1] = render(lastExecCount);
      if (timeline && timeline.lastElementChild) timeline.lastElementChild.outerHTML = render(lastExecCount);
    } else {
      lastExecKey = key;
      lastExecCount = 1;
      currentExecTrace.push(render(1));
      if (timeline) timeline.insertAdjacentHTML("beforeend", render(1));
    }
    if (timeline) timeline.scrollTop = timeline.scrollHeight;
  }

  function showExecBar() {
    const bar = document.getElementById("exec-status-bar");
    if (bar) bar.hidden = false;

    // Reset the per-response execution trace (Fix D). The global
    // "View/Hide Execution Details" toggle was removed; each response now
    // carries its own collapsible trace attached on completion.
    currentExecTrace = [];
    lastExecKey = null;
    lastExecCount = 1;
    streamBuf = "";
    streamSourceID = null;
    streamGenPending = false;

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
    // Mirror into the shared snapshot so a future view (e.g. Blueprint) can
    // read "what stage is the current turn in" without re-deriving it from
    // raw events itself.
    EventBus.currentActivity.stage = stage;
    EventBus.currentActivity.stageState = state;
  }

  function startExecTimer() {
    if (execTimer) clearInterval(execTimer);
    execTimer = setInterval(() => {
      // Ten DOM writes a second for a label nobody can see is pure waste in a
      // background tab; the elapsed time is recomputed from execStartTime on
      // the next tick anyway, so skipping is free.
      if (document.hidden) return;
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
    // Attach INSIDE the message body (the left-aligned column under the
    // bubble), NOT on the .msg flex row itself — appending to .msg makes the
    // panel a flex sibling of the avatar+body and it renders to the RIGHT of
    // the response ("location 1"). finalizeAssistantMessage attaches to
    // .msg-body, so both paths must agree so every response shows Execution
    // Details in the same place ("location 2", under the bubble).
    const bodyEl = msgEl.querySelector(".msg-body") || msgEl;
    bodyEl.appendChild(wrap);
  }

  // ── Consensus State ────────────────────────────────────────────────



  // ── Verification Pipeline State ────────────────────────────────────



  // ── Resource Monitor ───────────────────────────────────────────────


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



  // ── Structured pipeline-stage inference ────────────────────────────
  // Every EmitTaskUpdate(source, status, detail) call carries two enums the
  // backend itself controls (source → evt.task_id, status → evt.status) and
  // one free-text field a tool/LLM output can contain anything, including a
  // stage word by coincidence (e.g. editing plan.md tripped the old
  // content.includes("plan") branch, wrongly flashing "planning" while a file
  // tool ran). Stage inference below reads only the two enums — never
  // evt.content — closing that false-positive class entirely.
  const KERNEL_STEP_STAGE = { // kernel/orchestrator/kernel.go's k.log() step vocabulary
    plan: "planning", chat: "planning",
    budget: "retrieval", memory: "retrieval", store: "retrieval",
    compress: "compression", compression: "compression",
    execute: "execution", loop: "execution", model: "execution",
    merge: "execution", cascade: "execution", checkpoint: "execution", consensus: "execution",
    verify: "verification",
    observe: "reflection", improve: "reflection", escalate: "reflection", confidence: "reflection",
  };
  const LOOP_STATUS_STAGE = { // kernel/loop/loop.go's agentic-loop status vocabulary
    started: "execution", budget: "execution", thinking: "execution",
    verifying: "verification", acceptance: "verification",
    "self-eval": "reflection", critique: "reflection", reflect: "reflection",
    stuck: "execution", aborted: "completion", max_reached: "completion",
  };
  const STAGE_BY_SOURCE = { // remaining EmitTaskUpdate(source, ...) callers
    planner: "planning", router: "planning", direct: "planning", chat: "planning", general: "planning",
    executor: "execution", repair: "execution", consensus: "execution",
    verification: "verification",
  };
  const FAILED_STATUSES = new Set(["failed", "error", "aborted", "stuck", "rejected"]);
  const DONE_STATUSES = new Set(["completed", "done", "passed", "planned", "max_reached", "rolled_back"]);

  // stageForTaskUpdate resolves (stage, state) for a task_update event from
  // its source/status enums alone. Returns null when the event doesn't map to
  // one of the exec bar's 8 stages (e.g. "confidence" scoring, "kernel"/"cascade"
  // bookkeeping steps with no stage-relevant meaning).
  function stageForTaskUpdate(evt) {
    const source = String(evt.task_id || "");
    const status = String(evt.status || "");
    let stage = null;
    if (source === "kernel") stage = KERNEL_STEP_STAGE[status] || null;
    else if (source === "agentic-loop") stage = LOOP_STATUS_STAGE[status] || null;
    else if (source in STAGE_BY_SOURCE) stage = STAGE_BY_SOURCE[source];
    else if (source) stage = "execution"; // a DAG node id lands here directly
    if (!stage) return null;
    let state = "running";
    if (FAILED_STATUSES.has(status)) state = "failed";
    else if (DONE_STATUSES.has(status)) state = "completed";
    return { stage, state };
  }

  // ── Enhanced SSE Event Router ──────────────────────────────────────
  // handleV2Event sees every event via EventBus.onAny (below) — the direct
  // replacement for wrapping window.addEvent. That wrap chain was the reason
  // token_usage's tiles below never populated: 10-sse.js special-cased
  // token_usage and returned before ever calling the (possibly wrapped)
  // addEvent, so this switch's "case token_usage" was unreachable no matter
  // how it was wired in. 10-sse.js now emits every type unconditionally, so
  // this switch — unchanged below — actually runs for token_usage now.
  EventBus.onAny(handleV2Event);

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
        }
        break;
      case "compression":
        if (evt.content) {
          setExecStage("compression", "completed");
        }
        break;
      case "consensus":
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
          const rowLabel = `<span style="color:var(--text-mute)">[${timeStr}]</span> <span style="color:var(--accent-1)">[tool]</span> ${evt.tool} - ${msg}`;
          const loadingMsg = document.querySelector(".msg.loading");
          const timeline = loadingMsg ? loadingMsg.querySelector(".inline-exec-timeline") : null;
          if (timeline) timeline.hidden = false;
          // Coalesce repeats of the same tool+status into one ×N row.
          pushExecRow(timeline, "tool|" + evt.tool + "|" + msg, rowLabel);
        }
        break;
      case "file_change":
        handleFileChange(evt);
        break;
      case "plan_updated":
        pushSimpleExecRow("plan", "plan", "Updated plan");
        break;
      case "workflow_updated":
        pushSimpleExecRow("workflow", "workflow", "Updated workflow");
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

  // CHANGE_ICON/LABEL mirror the CLI's changeKindLabel (surfaces/cli/diff.go)
  // so a modified/created/deleted file reads the same way in both surfaces.
  const CHANGE_KIND = {
    file_create: { icon: "+", label: "created" },
    file_modify: { icon: "✎", label: "modified" },
    file_delete: { icon: "✗", label: "deleted" },
    command:     { icon: "$", label: "ran" },
    git:         { icon: "⎇", label: "git" },
  };

  // pushSimpleExecRow adds a plain labeled row (no tool/change payload) to
  // the live inline timeline on the current loading message — used for
  // plan/workflow updates so they show up the same way tool calls do,
  // instead of only appearing in the raw events panel as JSON.
  function pushSimpleExecRow(key, tag, text) {
    const timeStr = new Date().toLocaleTimeString();
    const rowLabel = `<span style="color:var(--text-mute)">[${timeStr}]</span> <span style="color:var(--accent-2)">[${tag}]</span> ${esc(text)}`;
    const loadingMsg = document.querySelector(".msg.loading");
    const timeline = loadingMsg ? loadingMsg.querySelector(".inline-exec-timeline") : null;
    if (timeline) timeline.hidden = false;
    pushExecRow(timeline, key + "|" + text, rowLabel);
  }

  // handleFileChange renders a file_change event (payload: core.Change) as a
  // readable "✎ modified path/to/file.go (ok)" row instead of leaving file
  // edits invisible in the live trace (previously file_change had no case in
  // this switch at all, so it never touched the exec bar or timeline).
  function handleFileChange(evt) {
    const c = (typeof evt.content === "object" && evt.content) ? evt.content : {};
    const kind = CHANGE_KIND[c.kind] || { icon: "•", label: c.kind || "change" };
    const target = c.path || c.command || "";
    const status = c.success === false ? "failed" : "ok";
    const text = `${kind.icon} ${kind.label} ${target} (${status})`;
    setExecStage("tools", "running");
    pushSimpleExecRow("file|" + target, "file", text);
  }

  // flushStreamedAnswer redraws .msg-stream-text from streamBuf, throttled to
  // one paint per animation frame regardless of how many chunks arrived —
  // re-rendering markdown on every individual token would be wasted work on
  // a fast stream.
  function flushStreamedAnswer() {
    streamFlushScheduled = false;
    const loadingMsg = document.querySelector(".msg.loading");
    const el = loadingMsg ? loadingMsg.querySelector(".msg-stream-text") : null;
    if (!el) return;
    el.innerHTML = renderMarkdown(streamBuf);
    const container = document.getElementById("chat-messages");
    if (container) container.scrollTop = container.scrollHeight;
  }

  function scheduleStreamFlush() {
    if (streamFlushScheduled) return;
    streamFlushScheduled = true;
    requestAnimationFrame(flushStreamedAnswer);
  }

  function handleTaskUpdate(evt) {
    const status = String(evt.status || "").toLowerCase();

    // Streaming token chunks are the live LLM output — the model composing
    // its answer in real time. They ARE the answer bubble's content while
    // it grows (Stream fix — this used to go to a separate "thinking" panel
    // nobody's response text ever visibly grew from). A new source (a
    // different task_id — a subagent taking over, or the general no-tools
    // path) or a fresh ReAct iteration (the "thinking" case below) starts a
    // new buffer: only the CURRENT generation's text is shown, never a
    // concatenation of abandoned earlier attempts.
    if (status === "streaming") {
      if (evt.content) {
        const source = evt.task_id || "";
        if (source !== streamSourceID || streamGenPending) {
          streamSourceID = source;
          streamGenPending = false;
          streamBuf = "";
        }
        streamBuf += evt.content;
        scheduleStreamFlush();
      }
      return;
    }
    if (status === "thinking" && evt.task_id === "agentic-loop") {
      // Marks a fresh ReAct iteration about to call the model — task_id
      // stays "agentic-loop" across every iteration, so this is the only
      // signal that the NEXT streaming chunk starts a new generation rather
      // than continuing the last one. Consumed by the next streaming chunk
      // above; falls through so the normal timeline/stage handling below
      // still runs for this event too.
      streamGenPending = true;
    }

    // Append to timeline (coalescing consecutive identical status rows).
    const timeStr = new Date(evt.timestamp || Date.now()).toLocaleTimeString();
    if (evt.content) {
      const rowLabel = `<span style="color:var(--text-mute)">[${timeStr}]</span> <span style="color:var(--accent-3)">[${evt.status || 'info'}]</span> ${evt.content}`;
      const loadingMsg = document.querySelector(".msg.loading");
      const timeline = loadingMsg ? loadingMsg.querySelector(".inline-exec-timeline") : null;
      if (timeline) timeline.hidden = false;
      pushExecRow(timeline, "task|" + (evt.status || "") + "|" + evt.content, rowLabel);
    }

    // Map task updates to pipeline stages — from the source/status enums
    // (see stageForTaskUpdate above), never from evt.content's free text.
    const resolved = stageForTaskUpdate(evt);
    if (resolved) {
      setExecStage(resolved.stage, resolved.state);
      // A stage's completion is also the next stage's start, for the two
      // transitions the exec bar visualizes as adjacent.
      if (resolved.state === "completed") {
        if (resolved.stage === "planning") setExecStage("retrieval", "running");
        else if (resolved.stage === "retrieval") setExecStage("execution", "running");
        else if (resolved.stage === "verification") setExecStage("reflection", "running");
      }
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

    EventBus.currentActivity.tokens = execTokenCount;
    EventBus.currentActivity.cost = cost;
    EventBus.currentActivity.latencyMs = latency;
    EventBus.currentActivity.contextTokens = prompt;

    // Store for per-message metadata
    lastMsgMeta = {
      model: stats.model || "—",
      provider: stats.provider || "—",
      tokens: total.toLocaleString(),
      cost: "$" + cost.toFixed(4),
      latency: latency > 0 ? latency + "ms" : "—"
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

