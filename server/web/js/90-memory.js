/* 90-memory.js — the Memory tab.
 *
 * Rewritten from a listing into a search.
 *
 * The old version called /api/memory and /api/knowledge on tab open, rendered
 * fifty rows per tier, and hung the browser: /api/knowledge answered "show me
 * the graph" with the entire graph, 16 MB of it. Paginating that would have
 * produced a smaller dump of a thing nobody reads by scrolling.
 *
 * Memory is a retrieval system, so this asks it questions. Nothing loads a
 * store on open — only counts, which are computed without sending contents.
 */

const MEM_SIGNAL_COLOR = {
  keyword: "var(--accent-3)",
  vector: "var(--cyan, #26c6da)",
  kg: "var(--green, #22c55e)",
};

// loadMemory is the tab's entry point. It fetches counts only: an empty query
// is the resting state, so opening the tab costs one small response.
async function loadMemory() {
  try {
    const res = await fetch(API + "/api/memory/search");
    renderMemCounts((await res.json()).counts || {});
  } catch (err) {
    const el = $("#mem-counts");
    if (el) el.textContent = "Memory unavailable: " + err.message;
  }
}

function renderMemCounts(c) {
  const el = $("#mem-counts");
  if (!el) return;
  const items = [
    ["conversation", "in this chat"],
    ["episodic", "past runs"],
    ["semantic", "facts"],
    ["procedural", "skills"],
    ["graph_nodes", "graph nodes"],
    ["graph_edges", "graph edges"],
  ];
  el.innerHTML = items
    .map(([k, label]) => `<span><b style="color:var(--text)">${fmtNum(c[k] || 0)}</b> ${label}</span>`)
    .join("");
}

async function searchMemory(q) {
  const body = $("#mem-results");
  if (!body) return;
  body.innerHTML = `<div class="mem-empty">Searching…</div>`;
  try {
    const res = await fetch(API + "/api/memory/search?q=" + encodeURIComponent(q));
    const data = await res.json();
    renderMemCounts(data.counts || {});
    const hits = data.hits || [];
    if (!hits.length) {
      body.innerHTML = `<div class="mem-empty">Nothing recalled for “${esc(q)}”.</div>`;
      return;
    }
    body.innerHTML = hits.map(renderMemHit).join("");
  } catch (err) {
    body.innerHTML = `<div class="mem-empty">Search failed: ${esc(err.message)}</div>`;
  }
}

// renderMemHit shows the signal that found each result. That is the whole
// reason this view is worth having: it makes the fusion observable instead of
// asserted, so a claim about the graph earning its keep can be watched.
function renderMemHit(h) {
  const sigs = String(h.signal || "").split("+").filter(Boolean);
  const badges = sigs
    .map((s) => `<span class="mem-tag" style="color:${MEM_SIGNAL_COLOR[s] || "var(--text-mute)"}">${esc(s)}</span>`)
    .join(" ");
  const when = h.timestamp ? fmtTime(h.timestamp) : "";
  return `
    <div class="mem-item" style="border-left: 2px solid var(--border); padding-left: 12px; margin-bottom: 14px;">
      <div style="display:flex; justify-content:space-between; gap:12px; align-items:baseline;">
        <div style="font-size:13px; color:var(--text);">${esc(h.goal || h.id || "(untitled)")}</div>
        <div style="flex:none; font-size:10px; color:var(--text-mute);">${badges} ${esc(h.source || "")} ${when}</div>
      </div>
      ${h.snippet ? `<div style="font-size:12px; color:var(--text-dim); margin-top:4px;">${esc(h.snippet)}</div>` : ""}
    </div>`;
}

function initMemorySearch() {
  const form = document.getElementById("mem-search-form");
  if (!form) return false;
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const q = (document.getElementById("mem-q").value || "").trim();
    if (q) searchMemory(q);
  });
  return true;
}

// The page fragment loads after this script, so wait for it the same way the
// other tab modules do.
(function () {
  if (initMemorySearch()) return;
  const obs = new MutationObserver(() => { if (initMemorySearch()) obs.disconnect(); });
  obs.observe(document.body, { childList: true, subtree: true });
})();
