/* 70-attachments.js — extracted from app.js (lines 1187-1586) */
// ATTACHMENTS — @-mention autocomplete + 📎 picker
//
// The chat input accepts file/folder attachments that are resolved
// server-side (attach.Resolve) into a markdown block prepended to the
// query. Two entry points feed the same chatAttachments[] state:
//   • the 📎 button — opens a multi-select browser anchored to the button
//   • typing @ in the textarea — opens the same browser filtered by the
//     token; the directory prefix of the token is browsed live so the
//     contents of any folder appear as options.
// ════════════════════════════════════════════════════════════════════════
let chatAttachments = [];          // [{type:"file"|"directory", path:"rel/path"}]
let attachBrowser = null;          // active browser context, or null
let attachBrowseCache = {};        // {dirPath: entries[]} short-lived cache

function isAttached(path) { return chatAttachments.some(a => a.path === path); }

function addAttachment(type, path) {
  if (!path || isAttached(path)) return;
  chatAttachments.push({ type, path });
  renderAttachTray();
  if (attachBrowser) renderAttachBrowserItems();
}

function removeAttachmentAt(idx) {
  chatAttachments.splice(idx, 1);
  renderAttachTray();
  if (attachBrowser) renderAttachBrowserItems();
}

function renderAttachTray() {
  const tray = $("#attach-tray");
  if (!tray) return;
  tray.innerHTML = "";
  chatAttachments.forEach((a, i) => {
    const chip = document.createElement("span");
    chip.className = "attach-chip";
    const name = a.path.split("/").pop() || a.path;
    chip.innerHTML =
      `<span class="ac-ico">${fileIcon(name, a.type === "directory")}</span>` +
      `<span class="ac-type">${a.type === "directory" ? "dir" : "file"}</span>` +
      `<span class="ac-name" title="${esc(a.path)}">${esc(name)}</span>` +
      `<span class="ac-x" title="Remove" data-idx="${i}">✕</span>`;
    tray.appendChild(chip);
  });
}

// ---------- workspace browsing ----------
async function browseWorkspaceDir(dir) {
  dir = dir || "";
  if (attachBrowseCache[dir]) return attachBrowseCache[dir];
  try {
    const url = API + "/api/workspace/browse" + (dir ? "?path=" + encodeURIComponent(dir) : "");
    const res = await fetch(url);
    if (!res.ok) return null;
    const data = await res.json();
    const entries = data.entries || [];
    attachBrowseCache[dir] = entries;
    // Drop the cache entry shortly after so fresh listings are fetched on
    // re-open (files may have changed).
    setTimeout(() => { delete attachBrowseCache[dir]; }, 4000);
    return entries;
  } catch {
    return null;
  }
}

// ---------- browser open/close ----------
function openAttachBrowser(mode, anchorEl) {
  const panel = $("#attach-browser");
  if (!panel) return;
  attachBrowseCache = {}; // fresh listings each open
  attachBrowser = {
    mode,           // "button" | "at"
    dir: "",        // currently browsed relative path
    entries: [],
    filter: "",
    activeIdx: 0,
    anchorEl,
    atRange: null,  // {start,end} of the @token in textarea (at-mode)
    loading: true,
  };
  panel.hidden = false;
  renderAttachBrowserShell();
  positionAttachBrowser();
  $("#chat-attach")?.classList.toggle("active", mode === "button");
  browserGo("");
}

function closeAttachBrowser() {
  const panel = $("#attach-browser");
  if (panel) panel.hidden = true;
  attachBrowser = null;
  $("#chat-attach")?.classList.remove("active");
}

async function browserGo(dir) {
  if (!attachBrowser) return;
  attachBrowser.dir = dir || "";
  attachBrowser.loading = true;
  attachBrowser.filter = "";
  attachBrowser.activeIdx = 0;
  renderAttachBrowserShell();
  const entries = await browseWorkspaceDir(attachBrowser.dir);
  if (!attachBrowser) return; // closed during fetch
  attachBrowser.entries = entries || [];
  attachBrowser.loading = false;
  renderAttachBrowserItems();
  positionAttachBrowser();
}

// ---------- browser rendering ----------
function renderAttachBrowserShell() {
  const panel = $("#attach-browser");
  if (!panel || !attachBrowser) return;
  const dir = attachBrowser.dir;
  const cwdLabel = dir ? `<span class="ab-cwd-root">workspace</span>/${esc(dir)}` : `<span class="ab-cwd-root">workspace</span>`;
  const filterAttrs = attachBrowser.mode === "at"
    ? ' readonly title="Filtering follows the @-token in the chat input"'
    : '';
  panel.innerHTML =
    `<div class="ab-head">` +
      `<button class="ab-up" id="ab-up" title="Up one level" ${dir ? "" : "disabled"}>↑</button>` +
      `<span class="ab-cwd" title="${esc(dir || "workspace root")}">${cwdLabel}</span>` +
      `<button class="ab-close" id="ab-close" title="Close (Esc)">✕</button>` +
    `</div>` +
    `<input class="ab-filter" id="ab-filter" placeholder="Filter…" autocomplete="off"${filterAttrs} />` +
    `<div class="ab-list custom-scrollbar" id="ab-list"></div>` +
    `<div class="ab-foot">` +
      `<span class="ab-count" id="ab-count"></span>` +
      `<span class="ab-hint">${attachBrowser.mode === "at" ? "↵ attach file · → open folder" : "click to attach · → open folder"}</span>` +
      `<button class="ab-attachdir" id="ab-attachdir" title="Attach the current folder" ${dir ? "" : "disabled"}>＋ folder</button>` +
    `</div>`;
  $("#ab-close")?.addEventListener("click", closeAttachBrowser);
  $("#ab-up")?.addEventListener("click", () => { if (attachBrowser && attachBrowser.dir) browserGo(parentRel(attachBrowser.dir)); });
  $("#ab-attachdir")?.addEventListener("click", () => {
    if (!attachBrowser || !attachBrowser.dir) return;
    addAttachment("directory", attachBrowser.dir);
    toast("success", "Attached folder: " + attachBrowser.dir);
    if (attachBrowser.mode === "at") commitAtSelection();
  });
  const filter = $("#ab-filter");
  if (filter) {
    filter.value = attachBrowser.filter || (attachBrowser.mode === "at" ? atFilterText() : "");
    filter.addEventListener("input", (e) => {
      if (!attachBrowser) return;
      attachBrowser.filter = e.target.value;
      attachBrowser.activeIdx = 0;
      renderAttachBrowserItems();
    });
  }
}

function renderAttachBrowserItems() {
  const list = $("#ab-list");
  const countEl = $("#ab-count");
  if (!list || !attachBrowser) return;
  if (attachBrowser.loading) {
    list.innerHTML = `<div class="ab-empty">Loading workspace…</div>`;
    if (countEl) countEl.textContent = "";
    return;
  }
  let entries = (attachBrowser.entries || []).slice();
  // In at-mode, the filter is derived from the @token unless the user typed
  // in the filter box. In button-mode, use the filter box.
  const filter = (attachBrowser.filter || "").toLowerCase().trim();
  if (filter) {
    entries = entries.filter(e => e.name.toLowerCase().includes(filter));
  }
  // Enforce an upper bound so huge directories stay responsive.
  const capped = entries.slice(0, 200);
  list.innerHTML = "";
  if (!capped.length) {
    list.innerHTML = `<div class="ab-empty">No matching files or folders.${entries.length > 200 ? "<br>(showing first 200)" : ""}</div>`;
    if (countEl) countEl.textContent = "";
    return;
  }
  capped.forEach((e, i) => {
    const row = document.createElement("div");
    const attached = isAttached(e.path);
    row.className = "ab-item" + (e.is_dir ? " is-dir" : "") + (attached ? " attached" : "") + (i === attachBrowser.activeIdx ? " active" : "");
    row.dataset.idx = String(i);
    row.innerHTML =
      `<span class="ab-ico">${fileIcon(e.name, e.is_dir)}</span>` +
      `<span class="ab-name" title="${esc(e.path)}">${esc(e.name)}</span>` +
      `<span class="ab-meta">${e.is_dir ? "folder" : fmtFileSize(e.size)}</span>` +
      `<span class="ab-check" title="Attached">✓</span>` +
      `<button class="ab-descend" title="Open folder">›</button>`;
    // Click the name/row → toggle attachment (file or folder).
    row.addEventListener("click", (ev) => {
      if (ev.target.closest(".ab-descend")) return;
      if (ev.target.closest(".ab-check")) return;
      toggleAttachEntry(capped[i]);
    });
    // The › button (or double-clicking a dir) descends into it.
    row.querySelector(".ab-descend")?.addEventListener("click", (ev) => {
      ev.stopPropagation();
      descendInto(capped[i]);
    });
    row.addEventListener("dblclick", () => { if (capped[i].is_dir) descendInto(capped[i]); });
    list.appendChild(row);
  });
  if (countEl) countEl.textContent = `${chatAttachments.length} attached`;
  // Keep the active row in view.
  const active = list.querySelector(".ab-item.active");
  if (active) active.scrollIntoView({ block: "nearest" });
}

function toggleAttachEntry(e) {
  if (!e) return;
  const type = e.is_dir ? "directory" : "file";
  if (isAttached(e.path)) {
    const idx = chatAttachments.findIndex(a => a.path === e.path);
    if (idx >= 0) removeAttachmentAt(idx);
  } else {
    addAttachment(type, e.path);
    if (attachBrowser && attachBrowser.mode === "at") {
      // Selecting a file from @ consumes the token and closes the picker.
      // Folders stay open so the user can descend or attach via the footer.
      if (!e.is_dir) commitAtSelection();
    }
  }
}

function descendInto(e) {
  if (!e || !e.is_dir || !attachBrowser) return;
  // In at-mode, extend the @token so the textarea reflects the browse path.
  if (attachBrowser.mode === "at" && attachBrowser.atRange) {
    setAtToken(attachBrowser.atRange, e.path + "/");
  }
  browserGo(e.path);
}

// ---------- positioning ----------
function positionAttachBrowser() {
  const panel = $("#attach-browser");
  if (!panel || !attachBrowser) return;
  const anchor = attachBrowser.anchorEl || $("#chat-text");
  if (!anchor) return;
  const r = anchor.getBoundingClientRect();
  const pw = Math.max(380, r.width || 380);
  panel.style.width = pw + "px";
  panel.style.left = r.left + "px";
  const aboveGap = r.top - 8;
  panel.style.top = "";
  panel.style.bottom = "";
  // Prefer above the input; fall back to below if there's no room.
  if (aboveGap > 180) {
    panel.style.bottom = (window.innerHeight - aboveGap) + "px";
  } else {
    panel.style.top = (r.bottom + 8) + "px";
  }
}

// ---------- @-mention detection ----------
function detectAtToken(ta) {
  const val = ta.value;
  const caret = ta.selectionStart;
  const before = val.slice(0, caret);
  const m = before.match(/(?:^|\s)@([^\s@]*)$/);
  if (!m) return null;
  const atIdx = before.length - m[0].length + (m[0].charAt(0) === "@" ? 0 : 1);
  return { start: atIdx, end: caret, token: m[1] };
}

function splitToken(token) {
  const slash = token.lastIndexOf("/");
  if (slash < 0) return { dir: "", filter: token };
  return { dir: token.slice(0, slash), filter: token.slice(slash + 1) };
}

function setAtToken(range, text) {
  const ta = $("#chat-text");
  if (!ta) return;
  ta.value = ta.value.slice(0, range.start) + "@" + text + ta.value.slice(range.end);
  const pos = range.start + 1 + text.length;
  ta.focus();
  ta.setSelectionRange(pos, pos);
  // Re-detect so atRange tracks the new caret.
  if (attachBrowser) {
    const next = detectAtToken(ta);
    attachBrowser.atRange = next;
  }
}

function atFilterText() {
  const ta = $("#chat-text");
  if (!ta || !attachBrowser) return "";
  const at = detectAtToken(ta);
  if (!at) return "";
  return splitToken(at.token).filter;
}

// Apply the @token's directory prefix as the browsed folder, and its tail as
// the filter. Called on every textarea input while an @token is active.
async function syncAtBrowse() {
  const ta = $("#chat-text");
  if (!ta) return;
  const at = detectAtToken(ta);
  if (!at) { if (attachBrowser && attachBrowser.mode === "at") closeAttachBrowser(); return; }
  const { dir, filter } = splitToken(at.token);
  if (!attachBrowser) {
    openAttachBrowser("at", ta);
    if (attachBrowser) attachBrowser.atRange = at;
  } else if (attachBrowser.mode !== "at") {
    return; // a button-mode picker is open; leave it alone
  }
  if (!attachBrowser) return;
  attachBrowser.atRange = at;
  attachBrowser.filter = filter;
  // Only re-fetch if the directory actually changed.
  if (attachBrowser.dir !== dir) {
    await browserGo(dir);
    if (!attachBrowser) return;
    attachBrowser.filter = filter;
    renderAttachBrowserItems();
  } else {
    const f = $("#ab-filter");
    if (f && document.activeElement !== f) f.value = filter;
    attachBrowser.activeIdx = 0;
    renderAttachBrowserItems();
  }
}

// Remove the @token from the textarea and close the picker (at-mode, after a
// file is attached).
function commitAtSelection() {
  const ta = $("#chat-text");
  if (ta && attachBrowser && attachBrowser.atRange) {
    const r = attachBrowser.atRange;
    ta.value = ta.value.slice(0, r.start) + ta.value.slice(r.end);
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, 200) + "px";
    ta.focus();
    const pos = r.start;
    ta.setSelectionRange(pos, pos);
  }
  closeAttachBrowser();
}

// ---------- keyboard navigation ----------
function onChatTextKeydown(e) {
  // Enter submits — but if an @-picker is open with an active dir/file, let
  // it consume the Enter instead.
  if (attachBrowser && attachBrowser.mode === "at") {
    const items = currentBrowserItems();
    if (e.key === "ArrowDown") { e.preventDefault(); moveBrowserSelection(1); return; }
    if (e.key === "ArrowUp")   { e.preventDefault(); moveBrowserSelection(-1); return; }
    if (e.key === "Escape")    { e.preventDefault(); closeAttachBrowser(); return; }
    if (e.key === "Enter" || e.key === "Tab") {
      const cur = items[attachBrowser.activeIdx];
      if (cur) {
        e.preventDefault();
        if (e.key === "Enter" && cur.is_dir) { descendInto(cur); return; }
        toggleAttachEntry(cur);
        return;
      }
    }
    if (e.key === "ArrowRight") {
      const cur = items[attachBrowser.activeIdx];
      if (cur && cur.is_dir) { e.preventDefault(); descendInto(cur); return; }
    }
  }
  // Ctrl+L / Cmd+L clears the chat screen (keeps memory) — like a terminal.
  if ((e.ctrlKey || e.metaKey) && (e.key === "l" || e.key === "L")) {
    e.preventDefault();
    $("#chat-clear")?.click();
    return;
  }

  // Default: Enter (no shift) sends the message.
  if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendChat(); }
}

function currentBrowserItems() {
  if (!attachBrowser || attachBrowser.loading) return [];
  const filter = (attachBrowser.filter || "").toLowerCase().trim();
  let entries = (attachBrowser.entries || []).slice();
  if (filter) entries = entries.filter(en => en.name.toLowerCase().includes(filter));
  return entries.slice(0, 200);
}

function moveBrowserSelection(delta) {
  if (!attachBrowser) return;
  const items = currentBrowserItems();
  if (!items.length) return;
  let idx = attachBrowser.activeIdx + delta;
  if (idx < 0) idx = items.length - 1;
  if (idx >= items.length) idx = 0;
  attachBrowser.activeIdx = idx;
  renderAttachBrowserItems();
}

function parentRel(rel) {
  if (!rel) return "";
  const i = rel.lastIndexOf("/");
  if (i < 0) return "";
  return rel.slice(0, i);
}

// ════════════════════════════════════════════════════════════════════════
