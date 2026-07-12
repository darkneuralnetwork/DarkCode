/* 90-memory.js — extracted from app.js (lines 1751-1842) */
// MEMORY
// ════════════════════════════════════════════════════════════════════════
async function loadMemory() {
  try {
    const res = await fetch(API + "/api/memory");
    const data = await res.json();
    renderMemList("mem-conversation", "mem-conversation-count", data.conversation, "message");
    renderMemList("mem-session", "mem-session-count", data.session, "state");
    renderMemList("mem-project", "mem-project-count", data.project, "episode");
    renderMemList("mem-workspace", "mem-workspace-count", data.workspace, "file");
    renderMemList("mem-user", "mem-user-count", data.user, "preference");
    renderMemList("mem-architecture", "mem-architecture-count", data.architecture, "fact");
  } catch (err) {
    ["conversation","session","project","workspace","user","architecture"].forEach((k) => {
      const el = $("#mem-" + k); if (el) el.innerHTML = `<div class="mem-empty">Failed: ${esc(err.message)}</div>`;
    });
  }
}

function renderMemList(bodyId, countId, items, label) {
  const body = $("#" + bodyId);
  const countEl = $("#" + countId);
  if (!body) return;
  const arr = Array.isArray(items) ? items : [];
  if (countEl) countEl.textContent = arr.length;
  if (!arr.length) { body.innerHTML = `<div class="mem-empty">No ${label}s stored.</div>`; return; }
  body.innerHTML = arr.slice(0, 50).map((item) => renderMemItem(item, label)).join("");
}

// renderMemItem renders a single memory entry in a readable, type-aware way
// instead of dumping raw JSON. Falls back to a stringified view for unknown shapes.
function renderMemItem(item, label) {
  if (typeof item === "string") return `<div class="mem-item">${esc(item)}</div>`;
  if (label === "fact") {
    // Semantic memory entry
    const cat = item.category ? `<span class="mem-tag">${esc(item.category)}</span>` : "";
    return `<div class="mem-item"><div class="mem-item-key">${esc(item.key || "")}</div>${cat}<div class="mem-item-body">${esc((item.content || "").replace(/\n/g, "<br>"))}</div></div>`;
  }
  if (label === "skill") {
    // Procedural memory (Skill)
    const steps = (item.steps || []).map((s) => `<li>${esc(s.action || "")}</li>`).join("");
    const meta = `use ${item.use_count || 0}× · ${Math.round((item.success_rate || 0) * 100)}% success`;
    return `<div class="mem-item"><div class="mem-item-key">${esc(item.name || "")}</div><div class="mem-item-meta">${meta}</div><div class="mem-item-body">${esc(item.description || "")}</div>${steps ? `<ol class="mem-steps">${steps}</ol>` : ""}</div>`;
  }
  if (label === "episode") {
    // Episodic memory entry
    const cls = item.outcome === "success" ? "mem-ok" : "mem-bad";
    const tools = (item.tools_used || []).join(", ");
    return `<div class="mem-item"><div class="mem-item-key">${esc(item.task_goal || "")}</div><span class="mem-tag ${cls}">${esc(item.outcome || "")}</span>${tools ? `<span class="mem-tag">${esc(tools)}</span>` : ""}<div class="mem-item-body">${esc((item.summary || "").replace(/\n/g, "<br>"))}</div></div>`;
  }
  // message / fallback
  const txt = (item && item.content != null) ? String(item.content) : JSON.stringify(item);
  const role = item && item.role ? `<span class="mem-tag">${esc(item.role)}</span>` : "";
  return `<div class="mem-item">${role}<div class="mem-item-body">${esc(txt)}</div></div>`;
}

// ════════════════════════════════════════════════════════════════════════
// KNOWLEDGE GRAPH
// ════════════════════════════════════════════════════════════════════════
async function loadKnowledgeGraph() {
  const nodesC = $("#kg-nodes");
  const edgesC = $("#kg-edges");
  if (!nodesC || !edgesC) return;
  nodesC.innerHTML = "<h3>Entities (Nodes)</h3><div class='mem-empty'>Loading...</div>";
  edgesC.innerHTML = "<h3>Relationships (Edges)</h3><div class='mem-empty'>Loading...</div>";
  try {
    const res = await fetch(API + "/api/knowledge");
    const data = await res.json();
    const stats = $("#kg-stats"); if (stats) stats.textContent = `${data.node_count || 0} nodes / ${data.edge_count || 0} edges`;
    nodesC.innerHTML = "<h3>Entities (Nodes)</h3>";
    if (!data.nodes || !data.nodes.length) {
      nodesC.innerHTML += "<div class='mem-empty'>No nodes in graph</div>";
    } else {
      data.nodes.forEach(n => {
        nodesC.innerHTML += `<div class="kg-item"><span class="kg-badge">${esc(n.type)}</span> <strong>${esc(n.id)}</strong><br><span style="color:var(--text-mute)">${esc(n.content || n.label || "")}</span></div>`;
      });
    }
    edgesC.innerHTML = "<h3>Relationships (Edges)</h3>";
    if (!data.edges || !data.edges.length) {
      edgesC.innerHTML += "<div class='mem-empty'>No edges in graph</div>";
    } else {
      data.edges.forEach(e => {
        edgesC.innerHTML += `<div class="kg-item"><strong>${esc(e.from)}</strong> <span style="color:var(--accent-3)">⟷</span> <strong>${esc(e.to)}</strong><br><span class="kg-badge" style="background:var(--bg-panel);margin-top:4px">${esc(e.relation)}</span> (weight: ${e.weight})</div>`;
      });
    }
  } catch(e) {
    nodesC.innerHTML = `<div class="mem-empty">Failed: ${esc(e.message)}</div>`;
    edgesC.innerHTML = `<div class="mem-empty">Failed: ${esc(e.message)}</div>`;
  }
}

// ════════════════════════════════════════════════════════════════════════
// PROJECT INTELLIGENCE
// ════════════════════════════════════════════════════════════════════════
async function loadProjectIntel() {
  try {
    const res = await fetch(API + "/api/intelligence/summary");
    if (!res.ok) return;
    const data = await res.json();
    
    if ($("#intel-files")) $("#intel-files").textContent = data.indexed_files || 0;
    if ($("#intel-symbols")) $("#intel-symbols").textContent = data.total_symbols || 0;
    if ($("#intel-funcs")) $("#intel-funcs").textContent = data.functions || 0;
    if ($("#intel-classes")) $("#intel-classes").textContent = data.types || 0;
    
    const depsC = $("#intel-deps");
    if (depsC) {
      if (data.packages > 0) {
        depsC.innerHTML = `<div style="font-size: 13px; color: var(--text-dim);">
          <div style="margin-bottom: 4px;"><strong>Packages:</strong> ${data.packages}</div>
          <div style="margin-bottom: 4px;"><strong>Call Edges:</strong> ${data.call_edges}</div>
          <div><strong>Language:</strong> <span class="kg-badge" style="background:var(--bg-panel);">${data.language}</span></div>
        </div>`;
      } else {
        depsC.innerHTML = `<div class='mem-empty'>No dependency data available.</div>`;
      }
    }
  } catch (e) {
    console.error("Failed to load project intelligence:", e);
  }
}
