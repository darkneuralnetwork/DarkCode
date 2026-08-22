/* 60-filetree.js — extracted from app.js (lines 832-1186) */
// LIVE WORKSPACE EXPLORER — current directory contents + agent modifications
// ════════════════════════════════════════════════════════════════════════
let fileTreeData = [];           // raw entries from /api/files/list
let fileTreeHash = "";           // signature to skip no-op re-renders
let fileModLog = [];             // [{path, action, time, tool}] most-recent-first
let modifiedPaths = {};          // { path: { action, ts } } for highlight
let collapsedDirs = new Set();   // collapsed directory paths
let fileTreePollTimer = null;
// D2: fileBaseline records the set of file paths that existed BEFORE the
// first chat run. It's populated lazily so that the first detectFileChanges
// call doesn't flag every pre-existing file as "created" (the old heuristic
// `!(path in modifiedPaths)` was broken because modifiedPaths was empty on
// the first run → every file looked new).
let fileBaseline = null;         // Set<string> | null — null = not yet populated
const FILE_TREE_POLL_MS = 4000;
const MOD_HIGHLIGHT_TTL = 90000;  // highlight modified files for 90s

const FILE_ICONS = {
  ".go": "🐹", ".js": "📜", ".ts": "📜", ".jsx": "⚛️", ".tsx": "⚛️",
  ".html": "🌐", ".css": "🎨", ".json": "📋", ".md": "📝", ".txt": "📄",
  ".py": "🐍", ".rs": "🦀", ".java": "☕", ".c": "🔧", ".cpp": "🔧",
  ".sh": "🐚", ".yml": "⚙️", ".yaml": "⚙️", ".toml": "⚙️", ".xml": "📰",
  ".sql": "🗄️", ".png": "🖼️", ".jpg": "🖼️", ".jpeg": "🖼️", ".gif": "🖼️",
  ".svg": "🖼️", ".lock": "🔒", ".mod": "📦", ".sum": "📦", ".env": "🔐",
};

function fileIcon(name, isDir) {
  if (isDir) return "📁";
  if (name === ".config" || name.endsWith(".config")) return "⚙️";
  const dot = name.lastIndexOf(".");
  if (dot >= 0) {
    const ext = name.slice(dot).toLowerCase();
    if (FILE_ICONS[ext]) return FILE_ICONS[ext];
  }
  return "📄";
}

function fmtFileSize(bytes) {
  if (bytes < 1024) return bytes + "B";
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + "K";
  return (bytes / 1048576).toFixed(1) + "M";
}

async function loadFileTree() {
  try {
    const res = await fetch(API + "/api/files/list");
    if (!res.ok) return;
    const data = await res.json();
    fileTreeData = data.entries || [];
    // D2: Seed the file baseline on the initial load so the first chat's
    // detectFileChanges has something to diff against (catches agent-created
    // files the SSE handler missed, without flagging pre-existing files).
    if (fileBaseline === null) {
      fileBaseline = new Set(fileTreeData.filter(e => !e.is_dir).map(e => e.path));
    }
    const cwdEl = $("#fe-cwd");
    if (cwdEl) { cwdEl.textContent = data.cwd || ""; cwdEl.title = data.cwd || ""; }
    renderFileTree();
  } catch (err) {
    const tree = $("#fe-tree");
    if (tree) tree.innerHTML = `<div class="fe-empty">Failed to load: ${esc(err.message)}</div>`;
  }
}

function refreshFileTree() {
  // Immediate re-fetch (used after a chat completes / tool fires).
  return loadFileTree();
}

// switchWorkspace prompts for a directory path and switches the chat console's
// file explorer to it. This is independent of project activation — useful for
// browsing a different folder without creating a project for it.
async function switchWorkspace(prefill) {
  const current = $("#fe-cwd")?.textContent || "";
  const path = prompt("Switch workspace to directory:", prefill || current);
  if (path === null) return; // cancelled
  try {
    const res = await fetch(API + "/api/workspace", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
    // D2: Reset the baseline on workspace switch — the new workspace has a
    // different file set, so the old baseline is stale.
    fileBaseline = null;
    await loadFileTree();
    toast("success", "Workspace: " + (data.path || path));
  } catch (err) {
    toast("error", "Switch failed: " + err.message);
  }
}

// Build a hierarchical tree from the flat entries list.
function buildTree(entries) {
  const root = { name: "", path: "", isDir: true, children: {} };
  for (const e of entries) {
    const parts = e.path.split("/");
    let node = root;
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i];
      const isLeaf = i === parts.length - 1;
      if (!node.children[part]) {
        node.children[part] = {
          name: part,
          path: parts.slice(0, i + 1).join("/"),
          isDir: isLeaf ? e.is_dir : true,
          size: isLeaf ? e.size : 0,
          mod_time: isLeaf ? e.mod_time : 0,
          ext: isLeaf ? e.ext : "",
          children: {},
        };
      }
      node = node.children[part];
    }
  }
  // Convert children maps to sorted arrays.
  function finalize(node) {
    const arr = Object.values(node.children);
    arr.sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
      return a.name.localeCompare(b.name);
    });
    node.childList = arr;
    arr.forEach(finalize);
  }
  finalize(root);
  return root;
}

function renderFileTree() {
  const treeEl = $("#fe-tree");
  if (!treeEl) return;
  if (!fileTreeData.length) {
    treeEl.innerHTML = '<div class="fe-empty">Workspace is empty.</div>';
    return;
  }

  // Signature to skip no-op re-renders (avoids flicker on polling).
  const sig = fileTreeData.map(e => e.path + ":" + e.mod_time + ":" + e.size).join("|") + "#" + JSON.stringify(modifiedPaths);
  if (sig === fileTreeHash && treeEl.children.length) return;
  fileTreeHash = sig;

  const root = buildTree(fileTreeData);
  const dirs = fileTreeData.filter(e => e.is_dir).length;
  const files = fileTreeData.length - dirs;
  const stats = $("#fe-stats");
  if (stats) stats.innerHTML = `<span><span class="fe-stat-num">${files}</span> files</span><span><span class="fe-stat-num">${dirs}</span> dirs</span><span><span class="fe-stat-num">${fileModLog.length}</span> mods</span>`;

  treeEl.innerHTML = "";
  root.childList.forEach((node) => treeEl.appendChild(renderTreeNode(node)));
  pruneCollapsed(root);
}

function pruneCollapsed(root) {
  // Remove collapsed-dir entries that no longer exist.
  const allPaths = new Set();
  (function collect(node) {
    allPaths.add(node.path);
    node.childList.forEach(collect);
  })(root);
  for (const p of [...collapsedDirs]) if (!allPaths.has(p)) collapsedDirs.delete(p);
}

function renderTreeNode(node) {
  const wrap = document.createElement("div");
  wrap.className = "fe-node";
  const row = document.createElement("div");
  const isDir = node.isDir;
  row.className = "fe-row " + (isDir ? "is-dir" : "is-file");

  const collapsed = isDir && collapsedDirs.has(node.path);
  if (isDir && !collapsed) row.classList.add("expanded");

  // Modification highlight
  const mod = modifiedPaths[node.path];
  if (mod) {
    row.classList.add("modified");
    if (Date.now() - mod.ts < 4000) row.classList.add("flash");
  }

  const chev = document.createElement("span");
  chev.className = "fe-chevron";
  chev.textContent = isDir ? "▶" : "";

  const icon = document.createElement("span");
  icon.className = "fe-icon";
  icon.textContent = fileIcon(node.name, isDir);

  const name = document.createElement("span");
  name.className = "fe-name";
  name.textContent = node.name;

  row.appendChild(chev);
  row.appendChild(icon);
  row.appendChild(name);

  if (mod) {
    const badge = document.createElement("span");
    badge.className = "fe-mod-badge " + (mod.action || "modified");
    badge.textContent = (mod.action || "MOD").slice(0, 4).toUpperCase();
    row.appendChild(badge);
  }

  if (!isDir) {
    const size = document.createElement("span");
    size.className = "fe-size";
    size.textContent = fmtFileSize(node.size);
    row.appendChild(size);
  }

  if (isDir) {
    row.addEventListener("click", () => {
      if (collapsedDirs.has(node.path)) collapsedDirs.delete(node.path);
      else collapsedDirs.add(node.path);
      fileTreeHash = ""; // force re-render
      renderFileTree();
    });
  } else {
    row.addEventListener("click", () => showFilePreview(node.path));
  }

  wrap.appendChild(row);

  if (isDir && node.childList && node.childList.length && !collapsed) {
    const kids = document.createElement("div");
    kids.className = "fe-children";
    node.childList.forEach((c) => kids.appendChild(renderTreeNode(c)));
    wrap.appendChild(kids);
  }
  return wrap;
}

// Snapshot current mod-times as a map path -> mod_time. Used for before/after
// comparison around a chat run to detect agent-made changes.
async function snapshotFileMods() {
  try {
    const res = await fetch(API + "/api/files/list");
    if (!res.ok) return {};
    const data = await res.json();
    const map = {};
    (data.entries || []).forEach((e) => { if (!e.is_dir) map[e.path] = e.mod_time; });
    return map;
  } catch { return {}; }
}

// Compare a before-snapshot to the current state and record any differences
// as agent modifications (created / modified). When beforeMap is null (P2:
// the pre-snapshot was removed to halve the per-send network cost), this is
// a best-effort single fetch that only records newly-created files not yet
// tracked in modifiedPaths — modified-existing files are already surfaced
// live by the SSE tool_execution handler, so we avoid a second snapshot.
async function detectFileChanges(beforeMap) {
  const after = await snapshotFileMods();
  let changed = 0;
  // D2: When there's no before-snapshot (the P2-optimized path), we can't
  // diff against the pre-run state. The authoritative source of agent-made
  // changes is the SSE handleFileToolEvent handler (fires live on
  // write_file/patch). This function is only a FALLBACK for changes the SSE
  // events missed. To do that correctly we need a baseline of what existed
  // before — so on the FIRST call we populate the baseline silently (no
  // highlights, no toast) and return. On subsequent calls we flag only files
  // NOT in the baseline as "created" (genuinely new).
  if (!beforeMap) {
    if (!fileBaseline || fileBaseline.size === 0) {
      // First run or empty baseline: establish the baseline silently.
      fileBaseline = new Set(Object.keys(after));
      return;
    }
    for (const [path] of Object.entries(after)) {
      if (!fileBaseline.has(path) && !(path in modifiedPaths)) {
        recordFileMod(path, "created", "agent");
        changed++;
      }
    }
    // Keep the baseline current so a file created then deleted doesn't
    // re-trigger as "created" on the next run.
    fileBaseline = new Set(Object.keys(after));
  } else {
    for (const [path, mtime] of Object.entries(after)) {
      if (!(path in beforeMap)) {
        recordFileMod(path, "created", "agent");
        changed++;
      } else if (mtime > beforeMap[path]) {
        recordFileMod(path, "modified", "agent");
        changed++;
      }
    }
  }
  if (changed) {
    refreshFileTree();
    toast("success", `✦ Agent modified ${changed} file${changed > 1 ? "s" : ""}`);
  }
}

// Handle a tool_execution SSE event: detect file operations and record them.
function handleFileToolEvent(evt) {
  const tool = evt.tool || "";
  const status = evt.status || "";
  if (!["write_file", "patch", "read_file"].includes(tool)) return;

  // The "requested" event carries the raw JSON arguments string in `content`.
  if (status === "requested") {
    let args = {};
    const raw = evt.content;
    if (typeof raw === "string") {
      try { args = JSON.parse(raw); } catch { /* not JSON */ }
    } else if (raw && typeof raw === "object") {
      args = raw;
    }
    const path = args.path || "";
    if (!path) return;
    let action = "modified";
    if (tool === "write_file") action = "created";
    else if (tool === "patch") action = "patched";
    else if (tool === "read_file") action = "read";
    recordFileMod(path, action, tool);
    // Live refresh so the new/changed file appears immediately.
    refreshFileTree();
  }
}

// Record a modification in the log + highlight map.
function recordFileMod(path, action, tool) {
  const norm = path.replace(/^\.\//, "");
  modifiedPaths[norm] = { action, ts: Date.now(), tool };
  fileModLog.unshift({ path: norm, action, time: new Date(), tool });
  if (fileModLog.length > 50) fileModLog.pop();
  renderModLog();
}

function renderModLog() {
  const list = $("#fe-mods-list");
  if (!list) return;
  if (!fileModLog.length) {
    list.innerHTML = '<div class="fe-mods-empty">No modifications yet.<br>Files changed by the agent appear here.</div>';
    return;
  }
  list.innerHTML = fileModLog.map((m) => `
    <div class="fe-mod-item ${m.action}" data-path="${esc(m.path)}">
      <span class="fe-mod-time">${fmtTime(m.time)}</span>
      <span class="fe-mod-act">${esc(m.action)}</span>
      <span class="fe-mod-path" title="${esc(m.path)}">${esc(m.path)}</span>
    </div>`).join("");
  list.querySelectorAll(".fe-mod-item").forEach((el) => {
    el.addEventListener("click", () => showFilePreview(el.dataset.path));
  });
}

// Periodically refresh the tree + expire old highlights.
// (P3) Skips the network fetch + re-render while the tab or document is
// hidden — the SSE tool_execution events already drive live updates, so the
// poll is only a fallback for changes made outside the agent.
function startFileTreePoll() {
  stopFileTreePoll();
  fileTreePollTimer = setInterval(() => {
    if (document.hidden) return;
    // Expire stale highlights.
    const now = Date.now();
    let expired = false;
    for (const [p, m] of Object.entries(modifiedPaths)) {
      if (now - m.ts > MOD_HIGHLIGHT_TTL) { delete modifiedPaths[p]; expired = true; }
    }
    if (expired) { fileTreeHash = ""; renderModLog(); }
    loadFileTree();
  }, FILE_TREE_POLL_MS);
}
function stopFileTreePoll() {
  if (fileTreePollTimer) { clearInterval(fileTreePollTimer); fileTreePollTimer = null; }
}

// File preview modal.
async function showFilePreview(path) {
  const modal = $("#file-viewer-modal");
  const title = $("#file-viewer-title");
  const body = $("#file-viewer-content");
  if (!modal || !body) return;

  modal.style.display = "flex";
  title.textContent = path;
  body.textContent = "Loading…";

  try {
    const res = await fetch(API + "/api/files/read?path=" + encodeURIComponent(path));
    const data = await res.json();
    if (data.error) {
      body.textContent = "Error: " + data.error;
      return;
    }
    body.textContent = data.content || "(empty file)";
    modal.dataset.path = path;
  } catch (err) {
    body.textContent = "Failed: " + err.message;
  }
}

function closeFilePreview() {
  const modal = $("#file-viewer-modal");
  if (modal) modal.style.display = "none";
}

// ════════════════════════════════════════════════════════════════════════
