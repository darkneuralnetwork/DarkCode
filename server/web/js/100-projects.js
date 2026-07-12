/* 100-projects.js — extracted from app.js (lines 1843-2304) */
// PROJECTS — long-lived per-project context (file-backed)
// Each project carries markdown notes (context.md) that are injected into
// the agent's prompt when the project is "active". The card grid lets you
// create, edit, open (activate), and delete projects.
// ════════════════════════════════════════════════════════════════════════

async function loadProjects() {
  const grid = $("#proj-grid");
  if (!grid) return;
  grid.innerHTML = '<div class="mem-empty" style="grid-column:1/-1">Loading…</div>';
  try {
    const res = await fetch(API + "/api/projects");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();
    const projects = data.projects || [];
    const stats = $("#proj-stats");
    if (stats) stats.textContent = `${projects.length} project${projects.length === 1 ? "" : "s"}`;
    renderProjects(projects);
  } catch (err) {
    const stats = $("#proj-stats"); if (stats) stats.textContent = "— projects";
    grid.innerHTML = `<div class="mem-empty" style="grid-column:1/-1">Failed to load: ${esc(err.message)}</div>`;
  }
}

function renderProjects(projects) {
  const grid = $("#proj-grid");
  if (!grid) return;
  if (!projects.length) {
    grid.innerHTML = `<div class="mem-empty" style="grid-column:1/-1">No projects yet. Click <strong>+ New Project</strong> to create one.<br><br>Projects store long-lived markdown context that is injected into the agent's prompt when activated.</div>`;
    return;
  }
  grid.innerHTML = projects.map(renderProjectCard).join("");
  // Wire up each card's action buttons. Listeners are attached fresh on
  // every render (old nodes + listeners are discarded with innerHTML).
  grid.querySelectorAll(".proj-card").forEach((card) => {
    const id = card.dataset.id;
    card.querySelectorAll(".proj-mini-btn").forEach((btn) => {
      btn.addEventListener("click", () => {
        const act = btn.dataset.act;
        if (act === "open") openProject(id);
        else if (act === "context") openContextEditor(id);
        else if (act === "edit") openProjectModal(id);
        else if (act === "delete") deleteProject(id);
      });
    });
  });
}

function renderProjectCard(p) {
  const isActive = p.id === activeProjectId;
  const tags = (p.tags || []).map((t) => `<span class="mem-tag">${esc(t)}</span>`).join("");
  const openLabel = isActive ? "Active ✓" : "Open";
  return `
    <div class="proj-card ${isActive ? "active" : ""}" data-id="${esc(p.id)}">
      <div class="proj-card-head">
        <span class="proj-card-ico">📁</span>
        <div style="flex:1;min-width:0;display:flex;flex-direction:column;gap:4px">
          <div class="proj-card-name">${esc(p.name)}</div>
          ${p.description ? `<div class="proj-card-desc">${esc(p.description)}</div>` : ""}
          ${p.path ? `<div class="proj-card-path" title="${esc(p.path)}">${esc(p.path)}</div>` : ""}
        </div>
      </div>
      ${tags ? `<div class="proj-card-tags">${tags}</div>` : ""}
      <div class="proj-card-foot">
        <button class="proj-mini-btn" data-act="open" ${isActive ? "disabled" : ""}>${openLabel}</button>
        <button class="proj-mini-btn" data-act="context">Context</button>
        <button class="proj-mini-btn" data-act="edit">Edit</button>
        <button class="proj-mini-btn" data-act="delete">Delete</button>
      </div>
      ${p.summary_len ? `<div class="proj-card-summary" title="A compressed context summary is available; it is injected into the prompt instead of the raw context.">✓ compressed summary</div>` : ""}
    </div>`;
}

// openProject activates a project (touches LastOpened + marks it active so
// its context.md is prepended to subsequent chat queries). It also switches
// the chat console's workspace directory to the project's path so the file
// explorer follows the project. The /open endpoint already switches the
// workspace server-side; here we re-apply it explicitly (so a page reload
// also works) and surface the result to the user.
async function openProject(id) {
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(id) + "/open", { method: "POST" });
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    const p = await res.json();
    // Switch the workspace to this project's path. The /open call already
    // did this server-side, but we re-assert it here so the result is
    // deterministic and we can report the actual workspace back.
    let wsPath = "";
    if (p && p.path) {
      try {
        const wsRes = await fetch(API + "/api/workspace", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path: p.path }),
        });
        const wsData = await wsRes.json().catch(() => ({}));
        if (wsRes.ok) wsPath = wsData.path || "";
        if (wsData.warning) toast("warning", wsData.warning);
      } catch {}
    }
    await setActiveProject(id);
    // setActiveProject already re-fetched + refreshed the file tree; show a
    // clear confirmation so the user knows the workspace followed.
    if (wsPath) {
      toast("success", `📂 Workspace switched to: ${wsPath}`);
    } else if (p && p.path) {
      toast("warning", "Project activated, but its working directory could not be opened.");
    } else {
      toast("success", "Project activated. No working directory set — add one via Edit to switch the workspace.");
    }
    loadProjects();
  } catch (err) {
    toast("error", "Activate failed: " + err.message);
  }
}

// --- New / Edit metadata modal ---

function openProjectModal(id) {
  const modal = $("#proj-modal");
  if (!modal) return;
  projEditingId = id || null;
  $("#proj-modal-title").textContent = id ? "Edit Project" : "New Project";
  // Reset first so stale values don't flash while we fetch.
  $("#pf-name").value = "";
  $("#pf-desc").value = "";
  $("#pf-path").value = "";
  $("#pf-tags").value = "";
  if (id) {
    // Pre-fill from the server (authoritative, includes path/tags).
    fetch(API + "/api/projects/" + encodeURIComponent(id))
      .then((r) => r.ok ? r.json() : Promise.reject(new Error("HTTP " + r.status)))
      .then((p) => {
        $("#pf-name").value = p.name || "";
        $("#pf-desc").value = p.description || "";
        $("#pf-path").value = p.path || "";
        $("#pf-tags").value = (p.tags || []).join(", ");
      })
      .catch((err) => toast("error", "Could not load project: " + err.message));
  }
  modal.classList.add("active");
  setTimeout(() => $("#pf-name")?.focus(), 60);
}

function closeProjectModal() {
  const modal = $("#proj-modal");
  if (modal) modal.classList.remove("active");
  projEditingId = null;
}

async function saveProject() {
  const name = ($("#pf-name").value || "").trim();
  if (!name) { toast("error", "Project name is required."); $("#pf-name")?.focus(); return; }
  const description = ($("#pf-desc").value || "").trim();
  const path = ($("#pf-path").value || "").trim();
  const tags = ($("#pf-tags").value || "").split(",").map((t) => t.trim()).filter(Boolean);

  const btn = $("#proj-save");
  const orig = btn ? btn.textContent : "Save";
  if (btn) { btn.disabled = true; btn.textContent = "Saving…"; }
  try {
    let res;
    if (projEditingId) {
      res = await fetch(API + "/api/projects/" + encodeURIComponent(projEditingId), {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, description, path, tags })
      });
    } else {
      res = await fetch(API + "/api/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, description, path, tags })
      });
    }
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    const savedData = await res.json();
    toast("success", projEditingId ? "Project updated." : "Project created.");
    closeProjectModal();
    await loadProjects();

    // Auto-activate and switch if created via Project Mode (or Loop Mode, which
    // is project-backed). pendingChatMode (default "project") controls whether
    // the mode stays "loop" after activation and which tab to land on.
    if (!projEditingId && window.projectModePending) {
      const pendingMode = window.pendingChatMode || "project";
      window.projectModePending = false;
      window.pendingChatMode = "project";
      setActiveProject(savedData.id);
      // Re-assert the mode: setActiveProject keeps "loop" for loop mode, but
      // for project mode it may have flipped to "project" — set explicitly.
      const mv = $("#chat-mode-value");
      if (mv) mv.value = pendingMode;
      const mb = $("#chat-mode-btn");
      if (mb) mb.title = "Chat Mode: " + (pendingMode === "loop" ? "Loop" : "Project") + " Mode";
      // Loop mode lands on chat (so the user can immediately start a loop);
      // project mode lands on the workflow tab (shows the seeded plan).
      switchTab(pendingMode === "loop" ? "nexus" : "blueprint");
      if (pendingMode === "loop") {
        toast("info", "Loop Mode active — project ready. The ReAct loop runs for each message.");
      }
    }
  } catch (err) {
    toast("error", "Save failed: " + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = orig; }
  }
}

async function deleteProject(id) {
  if (!confirm("Delete this project and its context? This cannot be undone.")) return;
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(id), { method: "DELETE" });
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    if (activeProjectId === id) await setActiveProject(null);
    toast("success", "Project deleted.");
    loadProjects();
  } catch (err) {
    toast("error", "Delete failed: " + err.message);
  }
}

// --- Context editor modal (the project's markdown notes) ---

async function openContextEditor(id) {
  const modal = $("#ctx-modal");
  if (!modal) return;
  ctxEditingId = id;
  const meta = $("#ctx-meta");
  const body = $("#ctx-body");
  if (body) body.value = "Loading…";
  if (meta) meta.textContent = "";
  try {
    // GET /api/projects/{id} returns metadata + the context body together.
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(id));
    if (!res.ok) throw new Error("HTTP " + res.status);
    const p = await res.json();
    $("#ctx-modal-title").textContent = "Context: " + p.name;
    if (meta) {
      meta.innerHTML = `<span class="mem-tag">${esc(p.id)}</span>` +
        (p.path ? `<span class="mem-tag" title="working directory">${esc(p.path)}</span>` : "") +
        (p.last_opened ? `<span class="mem-tag">opened ${esc(fmtTime(p.last_opened))}</span>` : "");
    }
    if (body) body.value = p.context || "";
  } catch (err) {
    if (body) body.value = "";
    if (meta) meta.textContent = "Failed to load: " + err.message;
  }
  modal.classList.add("active");
  setTimeout(() => body?.focus(), 60);
}

function closeContextEditor() {
  const modal = $("#ctx-modal");
  if (modal) modal.classList.remove("active");
  ctxEditingId = null;
}

// saveContext persists the markdown body. When setActive is true it also
// marks the project active (so the notes are injected into the next chat).
async function saveContext(setActive) {
  if (!ctxEditingId) return;
  const bodyEl = $("#ctx-body");
  const context = bodyEl ? bodyEl.value : "";
  const btn = setActive ? $("#ctx-set-active") : $("#ctx-save");
  const orig = btn ? btn.textContent : "Save";
  if (btn) { btn.disabled = true; btn.textContent = "Saving…"; }
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(ctxEditingId) + "/context", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ context })
    });
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    if (setActive) {
      await setActiveProject(ctxEditingId);
      toast("success", "Context saved & project activated.");
      closeContextEditor();
      loadProjects();
    } else {
      toast("success", "Context saved.");
    }
  } catch (err) {
    toast("error", "Save failed: " + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = orig; }
  }
}

// rewriteContext triggers an explicit local LLM rewrite of the project context.
async function rewriteContext() {
  if (!ctxEditingId) return;
  const btn = $("#ctx-rewrite");
  const orig = btn ? btn.textContent : "Rewrite";
  if (btn) { btn.disabled = true; btn.textContent = "Rewriting..."; }
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(ctxEditingId) + "/context/rewrite", {
      method: "POST"
    });
    if (!res.ok) { const d = await res.json().catch(() => ({})); throw new Error(d.error || "HTTP " + res.status); }
    toast("success", "Context rewritten successfully.");
    // Reload the editor with the new content
    openContextEditor(ctxEditingId);
  } catch (err) {
    toast("error", "Rewrite failed: " + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = orig; }
  }
}

// --- Active-project state + chat banner ---

// setActiveProject is the single source of truth for which project is
// "active" (its context.md is injected into chat). It persists the choice so
// it survives reloads and refreshes the chat-tab banner. It also switches
// the chat console's workspace directory to the project's path so the file
// explorer follows the project the user is working on.
async function setActiveProject(id) {
  activeProjectId = id || null;
  if (id) {
    try { localStorage.setItem("darkcode_active_project", id); } catch {}
    const modeVal = $("#chat-mode-value");
    // Don't clobber Loop mode: loop is a project-bearing mode (project path +
    // context + plan/workflow + the ReAct loop), so activating a project from
    // loop mode must KEEP the mode as "loop". Only force "project" when the
    // current mode is general/auto/blank.
    if (modeVal && modeVal.value !== "project" && modeVal.value !== "loop") {
      modeVal.value = "project";
      const modeBtn = $("#chat-mode-btn");
      if (modeBtn) modeBtn.title = "Chat Mode: Project Mode";
    }
  } else {
    try { localStorage.removeItem("darkcode_active_project"); } catch {}
  }
  await updateProjectBanner();
  // The workspace switch happens inside updateProjectBanner (it already
  // fetches the project), then refresh the file explorer so the new cwd
  // is visible immediately.
  await loadFileTree();
  if (id) {
    await fetchProjectPlanAndWorkflow(id);
  } else {
    // Clear workflow tab (legacy elements; guarded for the consolidated
    // Blueprint page which no longer defines them).
    const eE = $("#workflow-empty"); if (eE) eE.style.display = "block";
    const cE = $("#workflow-content"); if (cE) cE.style.display = "none";
  }
}

async function fetchProjectPlanAndWorkflow(id) {
  // The consolidated Blueprint tab (blueprint.html) only exposes
  // #workflow-plan-view / #workflow-arch-view. The legacy #workflow-empty,
  // #workflow-content, #workflow-proj-name elements lived in the old
  // workflow.html and were not carried over — guard them so this function
  // never throws a null-deref before reaching the fetches (root cause of the
  // empty Plan & Workflow views).
  const emptyEl = $("#workflow-empty");
  const contentEl = $("#workflow-content");
  const nameEl = $("#workflow-proj-name");
  if (emptyEl) emptyEl.style.display = "none";
  if (contentEl) contentEl.style.display = "block";
  if (nameEl) nameEl.textContent = activeProjectName || "Active Project";

  try {
    const planRes = await fetch(API + "/api/projects/" + encodeURIComponent(id) + "/plan");
    if (planRes.ok) {
      const p = await planRes.json();
      const planView = $("#workflow-plan-view");
      if (planView) {
        planView.dataset.raw = p.plan || "";
        renderPlanBoard(p.plan || "", planView, "plan");
      }
    }
    const wfRes = await fetch(API + "/api/projects/" + encodeURIComponent(id) + "/workflow");
    if (wfRes.ok) {
      const w = await wfRes.json();
      const archView = $("#workflow-arch-view");
      if (archView) {
        archView.dataset.raw = w.workflow || "";
        renderPlanBoard(w.workflow || "", archView, "workflow");
      }
    }
  } catch (e) {
    console.error("Failed to fetch plan/workflow", e);
  }
}

// updateProjectBanner fetches the active project's name + context length and
// reflects them in the chat-tab toolbar (project dropdown label). It also
// drives the workspace switch: when a project is active, the chat console
// browses that project's path; when none is active, the workspace reverts to
// the server's process cwd. If the project no longer exists, the activation
// is cleared automatically.
async function updateProjectBanner() {
  const label = $("#ct-project-label");
  if (!activeProjectId) {
    if (label) label.textContent = "No Project";
    activeProjectName = "";
    activeContextLen = 0;
    // Revert the workspace to the process cwd now that no project owns it.
    try {
      await fetch(API + "/api/workspace", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: "" }),
      });
    } catch {}
    return;
  }
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(activeProjectId));
    if (!res.ok) {
      // Project was deleted or is unreachable — deactivate quietly.
      activeProjectId = null;
      try { localStorage.removeItem("darkcode_active_project"); } catch {}
      if (label) label.textContent = "No Project";
      activeProjectName = "";
      activeContextLen = 0;
      return;
    }
    const p = await res.json();
    activeProjectName = p.name || "";
    activeContextLen = (p.context || "").length;
    // Switch the chat workspace to this project's path. The server validates
    // the path; an invalid/missing path leaves the workspace as-is. We do
    // this here (rather than only in /open) so a page reload re-applies the
    // workspace from the persisted active-project id.
    if (p.path) {
      try {
        await fetch(API + "/api/workspace", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path: p.path }),
        });
      } catch {}
    }
    // Update the toolbar project dropdown label (replaces the old banner).
    if (label) {
      const shortName = activeProjectName.length > 18
        ? activeProjectName.slice(0, 17) + "…"
        : activeProjectName;
      label.textContent = shortName || "Project";
      label.title = activeProjectName + (activeContextLen > 0
        ? ` · ${activeContextLen.toLocaleString()} chars in context`
        : " · no context saved yet");
    }
  } catch {
    // Transient network error — leave the banner as-is.
  }
}

// appendToProjectContext appends a concise session entry (Q + A) to the
// active project's context.md so the project accumulates knowledge across
// queries. It also refreshes the Projects tab / open editor + banner so the
// UI stays in sync. Best-effort: failures are logged, never surfaced.
async function appendToProjectContext(id, question, answer) {
  if (!id) return;
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(id));
    if (!res.ok) return;
    const p = await res.json();
    const prev = p.context || "";
    const stamp = new Date().toLocaleString();
    const q = (question || "").trim().slice(0, 1000);
    const a = (answer || "").trim().slice(0, 2000);
    let entry = "\n\n---\n\n## Session · " + stamp + "\n\n**Q:** " + q + "\n";
    if (a) entry += "\n**A:** " + a + "\n";
    let next = prev.replace(/\s+$/, "") + entry;
    // Keep the context bounded (the server also caps at 1 MiB).
    const MAX = 1 << 20;
    if (next.length > MAX) next = next.slice(next.length - MAX);
    const put = await fetch(API + "/api/projects/" + encodeURIComponent(id) + "/context", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ context: next })
    });
    if (!put.ok) return;
    activeContextLen = next.length;
    // If the context editor is open for this project, refresh its textarea.
    if (ctxEditingId === id && $("#ctx-modal") && $("#ctx-modal").classList.contains("active")) {
      const bodyEl = $("#ctx-body");
      if (bodyEl) bodyEl.value = next;
    }
    // Refresh the Projects grid so the updated context is visible.
    if (activeTab === "projects") loadProjects();
    await updateProjectBanner();
  } catch (err) {
    console.warn("project context sync failed", err);
  }
}

// appendSystemNote inserts a slim, muted, centered note into the chat stream
// (used to signal e.g. "project context injected").
function appendSystemNote(text) {
  const container = $("#chat-messages");
  if (!container) return;
  const el = document.createElement("div");
  el.className = "msg system-note";
  el.textContent = text;
  container.appendChild(el);
  container.scrollTop = container.scrollHeight;
}

// ════════════════════════════════════════════════════════════════════════

// ════════════════════════════════════════════════════════════════════════
// BLUEPRINT TASK BOARD — catchy plan/workflow renderer with live progress,
// clickable checkbox toggles, and real-time SSE updates.
// ════════════════════════════════════════════════════════════════════════

// renderPlanBoard parses markdown containing "- [ ]" / "- [x]" task lines and
// renders a task board with clickable checkboxes, status pills, section
// grouping, and a live progress bar in the panel header. Non-checkbox lines
// fall back to markdown rendering so plans without tasks still display.
// `kind` is "plan" or "workflow" (drives the progress fill/label element ids).
function renderPlanBoard(markdown, container, kind) {
  if (!container) return;
  markdown = markdown || "";
  if (!markdown.trim()) {
    container.innerHTML = '<p style="color: var(--text-mute);"><i>Awaiting ' + kind + ' generation...</i></p>';
    updateBoardProgress(kind, 0, 0);
    return;
  }

  // Detect checkbox lines. If none, fall back to plain markdown.
  const lines = markdown.split("\n");
  const hasTasks = lines.some(l => /^\s*[-*]\s+\[[ xX]\]\s+/.test(l));
  if (!hasTasks) {
    container.innerHTML = renderMarkdown(markdown);
    if (window.mermaid) {
      try { mermaid.init(undefined, container.querySelectorAll('.mermaid')); } catch (e) {}
    }
    updateBoardProgress(kind, 0, 0);
    return;
  }

  // Build task rows grouped by the most recent heading.
  let html = "";
  let sectionTitle = "";
  let done = 0, total = 0;

  for (const raw of lines) {
    const line = raw.trimEnd();
    // Heading → section divider.
    const headingMatch = line.match(/^(#{1,4})\s+(.*)$/);
    if (headingMatch) {
      sectionTitle = headingMatch[2].trim();
      html += '<div class="bp-section">' + esc(sectionTitle) + '</div>';
      continue;
    }
    // Checkbox task line.
    const taskMatch = line.match(/^(\s*)[-*]\s+\[([ xX\/])\]\s+(.*)$/);
    if (taskMatch) {
      const stateChar = taskMatch[2].toLowerCase();
      const checked = stateChar === "x";
      const running = stateChar === "/";
      const text = taskMatch[3];
      const indent = taskMatch[1].length;
      total++;
      if (checked) done++;
      
      let statusClass = "bp-status-todo";
      let statusText = "TODO";
      let taskClass = "bp-task";
      let checkIcon = "";
      if (checked) {
          statusClass = "bp-status-done";
          statusText = "DONE";
          taskClass = "bp-task done";
          checkIcon = "✓";
      } else if (running) {
          statusClass = "bp-status-running";
          statusText = "RUNNING";
          taskClass = "bp-task running";
          checkIcon = "⟳";
      }

      html += '<div class="' + taskClass + '" style="margin-left:' + (indent * 16) + 'px">' +
        '<label class="bp-check" data-kind="' + kind + '">' +
          '<input type="checkbox" ' + (checked ? "checked" : "") + ' />' +
          '<span class="bp-check-box">' + checkIcon + '</span>' +
        '</label>' +
        '<span class="bp-task-text">' + renderInlineMd(text) + '</span>' +
        '<span class="bp-status ' + statusClass + '">' + statusText + '</span>' +
      '</div>';
      continue;
    }
    // Non-empty, non-task, non-heading line → render as a note.
    if (line.trim()) {
      html += '<div class="bp-note">' + renderInlineMd(line) + '</div>';
    }
  }

  container.innerHTML = html;
  if (window.mermaid) {
    try { mermaid.init(undefined, container.querySelectorAll('.mermaid')); } catch (e) {}
  }
  container.dataset.raw = markdown;
  updateBoardProgress(kind, done, total);

  // Wire checkbox toggles.
  container.querySelectorAll(".bp-check input[type=checkbox]").forEach((cb, i) => {
    cb.addEventListener("change", () => togglePlanTask(container, kind, i));
  });
}

// renderInlineMd renders a single line with minimal inline markdown (bold,
// italic, code) without block-level wrapping. Used inside task rows.
function renderInlineMd(text) {
  let s = esc(text);
  s = s.replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/`([^`]+)`/g, '<code>$1</code>');
  s = s.replace(/\*([^*]+?)\*/g, "<em>$1</em>");
  return s;
}

// updateBoardProgress updates the header progress bar + label for a board.
function updateBoardProgress(kind, done, total) {
  const label = $("#" + kind + "-progress-label");
  const fill = $("#" + kind + "-progress-fill");
  if (label) label.textContent = done + "/" + total;
  if (fill) fill.style.width = total > 0 ? ((done / total) * 100) + "%" : "0%";
}

// togglePlanTask flips the [ ]/[x] state of the i-th checkbox line in the
// board's source markdown and PUTs it to the server. The SSE
// plan_updated/workflow_updated event (or the PUT response) then re-renders.
function togglePlanTask(container, kind, index) {
  const raw = container.dataset.raw || "";
  const lines = raw.split("\n");
  let count = 0;
  for (let i = 0; i < lines.length; i++) {
    const m = lines[i].match(/^(\s*)[-*]\s+\[([ xX])\]\s+(.*)$/);
    if (!m) continue;
    if (count === index) {
      const isChecked = m[2].toLowerCase() === "x";
      lines[i] = m[1] + "- [" + (isChecked ? " " : "x") + "] " + m[3];
      break;
    }
    count++;
  }
  const next = lines.join("\n");
  container.dataset.raw = next;
  // PUT to the server; re-render locally for immediate feedback.
  renderPlanBoard(next, container, kind);
  if (!activeProjectId) return;
  const endpoint = kind === "plan" ? "plan" : "workflow";
  fetch(API + "/api/projects/" + encodeURIComponent(activeProjectId) + "/" + endpoint, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(kind === "plan" ? { plan: next } : { workflow: next }),
  }).catch(e => console.warn("toggle task PUT failed", e));
}

// regeneratePlanOrWorkflow triggers a server-side re-generation of the
// plan or workflow for the active project via the regenerate endpoint.
async function regeneratePlanOrWorkflow(kind) {
  if (!activeProjectId) { toast("warn", "Activate a project first"); return; }
  toast("info", "Regenerating " + kind + "…");
  try {
    const res = await fetch(API + "/api/projects/" + encodeURIComponent(activeProjectId) + "/" + kind + "/regenerate", {
      method: "POST",
    });
    if (!res.ok) throw new Error("status " + res.status);
    toast("ok", kind + " regeneration queued");
  } catch (e) {
    toast("error", "Failed to regenerate " + kind + ": " + e.message);
  }
}
