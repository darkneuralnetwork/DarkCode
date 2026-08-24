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
 *
 * Tier filtering: /api/memory/search's hybrid retriever only searches
 * episodic + semantic (memory/memory/retrieval.go's Recall never touches
 * procedural or the raw graph — the graph is a ranking boost, not a result
 * source). Procedural memory (learned strategies) is a different shape of
 * data entirely (task_type/name/success_count, not goal/snippet/signal), so
 * "Procedural" filters that list client-side by the same query text instead
 * of pretending it went through Recall. Conversation and Graph have counts
 * only — this tab deliberately never dumps either wholesale (see
 * handleMemorySearch's doc comment) — so those tiers show the count and stop.
 */

const MEM_SIGNAL_COLOR = {
  keyword: "var(--accent-3)",
  vector: "var(--cyan, #26c6da)",
  kg: "var(--green, #22c55e)",
};

let memCounts = {};
let memHits = [];       // last search's episodic+semantic hits
let memQuery = "";
let memTier = "all";    // all | episodic | semantic | procedural | conversation | graph
let strategies = null;  // learned strategies, fetched once and cached
let strategyStats = null;

// loadMemory is the tab's entry point. It fetches counts (and, once, the
// learned-strategies list) only: an empty query is the resting state, so
// opening the tab costs two small responses, neither a store dump.
async function loadMemory() {
  try {
    const res = await fetch(API + "/api/memory/search");
    memCounts = (await res.json()).counts || {};
  } catch (err) {
    const el = $("#mem-counts");
    if (el) el.textContent = "Memory unavailable: " + err.message;
  }
  await ensureStrategies();
  renderMemCounts();
  renderResults();
}

async function ensureStrategies() {
  if (strategies) return;
  try {
    const res = await fetch(API + "/api/learning/stats");
    const data = await res.json();
    strategies = data.strategies || [];
    strategyStats = data.stats || null;
  } catch {
    strategies = [];
  }
}

function renderMemCounts() {
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
  let html = items
    .map(([k, label]) => `<span><b style="color:var(--text)">${fmtNum(memCounts[k] || 0)}</b> ${label}</span>`)
    .join("");
  if (strategyStats) {
    html += `<span><b style="color:var(--text)">${fmtNum(strategyStats.total_tasks || 0)}</b> tasks learned from (${strategyStats.success_rate || 0}% success)</span>`;
  }
  el.innerHTML = html;
}

async function searchMemory(q) {
  memQuery = q;
  const body = $("#mem-results");
  if (!body) return;
  body.innerHTML = `<div class="mem-empty">Searching…</div>`;
  try {
    const res = await fetch(API + "/api/memory/search?q=" + encodeURIComponent(q));
    const data = await res.json();
    memCounts = data.counts || {};
    memHits = data.hits || [];
    renderMemCounts();
    renderResults();
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

function renderStrategyTile(s) {
  return `
    <div class="s-tile">
      <div class="s-tile-label">${esc(s.task_type)} strategy</div>
      <div style="font-size:14px; font-weight:600; margin-bottom:8px">${esc(s.name)}</div>
      <div style="font-size:12px; color:var(--text-dim); margin-bottom:12px">${esc(s.description)}</div>
      <div style="display:flex; justify-content:space-between; margin-bottom:8px">
         <span style="font-size:11px; color:var(--green)">Success: ${s.success_count}</span>
         <span style="font-size:11px; color:var(--red)">Fail: ${s.fail_count}</span>
      </div>
      <div class="s-layers" style="margin-top:8px">
        ${(s.preferred_tools || []).map(t => `<span class="s-layer" style="font-size:10px; padding:2px 6px;">${esc(t)}</span>`).join("")}
      </div>
    </div>`;
}

// matchingStrategies filters the cached learned-strategies list by the
// current query — the only way "Procedural" behaves like a search result
// rather than a static list, since Recall() never indexes this store.
function matchingStrategies() {
  const list = strategies || [];
  const q = memQuery.trim().toLowerCase();
  if (!q) return list;
  return list.filter((s) =>
    [s.task_type, s.name, s.description].some((f) => String(f || "").toLowerCase().includes(q)));
}

// renderResults is the tab's single render path: whatever changed (a new
// search, a tier click), this redraws #mem-results from the cached state
// (memHits, strategies, memCounts) — no tier switch ever re-fetches.
function renderResults() {
  const body = $("#mem-results");
  if (!body) return;

  if (memTier === "conversation" || memTier === "graph") {
    const label = memTier === "conversation" ? "this conversation" : "the knowledge graph";
    const count = memTier === "conversation" ? (memCounts.conversation || 0)
      : `${fmtNum(memCounts.graph_nodes || 0)} nodes / ${fmtNum(memCounts.graph_edges || 0)} edges`;
    body.innerHTML = `<div class="mem-empty">${esc(String(count))} in ${label}. Not browsable here — ask a question and the graph still shapes which episodic/semantic results rank first.</div>`;
    return;
  }

  const showEpisodic = memTier === "all" || memTier === "episodic";
  const showSemantic = memTier === "all" || memTier === "semantic";
  const showProcedural = memTier === "all" || memTier === "procedural";

  const hits = memHits.filter((h) =>
    (h.source === "episodic" && showEpisodic) || (h.source === "semantic" && showSemantic));
  const strats = showProcedural ? matchingStrategies() : [];

  if (!memQuery && memTier === "all") {
    body.innerHTML = `<div class="mem-empty">Search to see what the agent remembers.</div>`;
    return;
  }

  const sections = [];
  if (hits.length) sections.push(hits.map(renderMemHit).join(""));
  if (strats.length) {
    const heading = memQuery ? "Matching learned strategies" : "Learned strategies";
    sections.push(`<div class="chart-sub" style="margin:${sections.length ? "18px" : "0"} 0 10px;">${esc(heading)}</div><div class="s-grid">${strats.map(renderStrategyTile).join("")}</div>`);
  }

  if (!sections.length) {
    const q = memQuery ? `“${esc(memQuery)}”` : "this tier";
    body.innerHTML = `<div class="mem-empty">Nothing recalled for ${q}.</div>`;
    return;
  }
  body.innerHTML = sections.join("");
}

function initMemorySearch() {
  const form = document.getElementById("mem-search-form");
  const filter = document.getElementById("mem-tier-filter");
  if (!form || !filter) return false;
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const q = (document.getElementById("mem-q").value || "").trim();
    if (q) searchMemory(q);
    else { memQuery = ""; memHits = []; renderResults(); }
  });
  filter.addEventListener("click", (e) => {
    const btn = e.target.closest(".log-cat-tab");
    if (!btn) return;
    filter.querySelectorAll(".log-cat-tab").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    memTier = btn.dataset.tier || "all";
    if (memTier === "procedural") ensureStrategies().then(renderResults);
    else renderResults();
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
