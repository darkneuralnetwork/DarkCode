/* 40-events.js — extracted from app.js (lines 489-681) */
// EVENTS STREAM UI
// ════════════════════════════════════════════════════════════════════════
// evtContentText renders an event's `content` field as a displayable string.
// The server emits structured objects for some events (e.g. tool_execution
// carries a *ToolResult, file_change carries a Change struct); the previous
// `String(content || "")` turned those into "[object Object]" in the events
// panel. Objects are now JSON-stringified (and truncated) for readability.
function evtContentText(c) {
  if (c === null || c === undefined) return "";
  if (typeof c === "string") return c;
  if (typeof c === "object") {
    try {
      let s = JSON.stringify(c);
      if (s.length > 800) s = s.slice(0, 800) + "…";
      return s;
    } catch (e) { return String(c); }
  }
  return String(c);
}

// Event coalescing state — tracks the last group row so consecutive
// same-type events merge into one collapsible row instead of flooding the
// feed with one DOM element per streaming chunk (a single response can emit
// 200+ task_update chunks). Reset on clear.
let lastGroupEl = null;

// Types that are expanded by default (significant). Everything else collapses
// to a count summary — click the chevron to expand. Streaming chunks always
// coalesce into a single live-updating row (no per-chunk DOM).
const EVT_EXPANDED_TYPES = new Set(["error", "approval", "final_output"]);

function addEvent(evt) {
  const list = $("#events-list");
  if (!list) return;
  const empty = list.querySelector(".events-empty");
  if (empty) empty.remove();

  evtCount++;
  const c = $("#evt-count"); if (c) c.textContent = evtCount;
  if (activeTab !== "events") showEvtBadge(evtCount);

  let eType = evt.type || "unknown";
  const eTask = evt.task || evt.agent || "";

  // Enterprise UI Event Mapping
  if (eType === "task_update") {
    if (eTask === "verification") eType = "verification_pipeline";
    if (eTask === "router") eType = "router_decision";
    if (eTask === "security-sandbox") eType = "security_sandbox";
  }
  const isStreaming = evt.status === "streaming";

  // ── Streaming coalescing: if this is a streaming chunk for the same task as
  // the last group, update that row's preview text in-place. No new DOM
  // element, no reflow — this is the key perf win (200 chunks → 1 row).
  if (isStreaming && lastGroupEl && lastGroupEl.dataset.streaming === "1"
      && lastGroupEl.dataset.task === eTask) {
    const preview = lastGroupEl.querySelector(".evt-content");
    if (preview) {
      // Accumulate streaming text so the preview grows; cap to avoid a giant
      // text node.
      const text = evtContentText(evt.content);
      const cur = preview.dataset.acc || "";
      const acc = (cur + text).slice(-2000);
      preview.dataset.acc = acc;
      preview.textContent = acc.slice(-400) + (acc.length > 400 ? "…" : "");
    }
    const timeEl = lastGroupEl.querySelector(".evt-time");
    if (timeEl) timeEl.textContent = fmtTime(evt.timestamp);
    if ($("#evt-autoscroll") && $("#evt-autoscroll").checked) list.scrollTop = list.scrollHeight;
    return;
  }

  // ── Same-type grouping: if the last group is the same type + task (and not
  // a completed streaming row), append to it + bump the count.
  if (lastGroupEl && lastGroupEl.dataset.type === eType
      && lastGroupEl.dataset.task === eTask
      && lastGroupEl.dataset.streaming !== "1") {
    const details = lastGroupEl.querySelector(".evt-group-details");
    if (details) details.appendChild(buildEventDetail(evt));
    const cntEl = lastGroupEl.querySelector(".evt-count");
    if (cntEl) {
      const n = parseInt(cntEl.dataset.n || "1", 10) + 1;
      cntEl.dataset.n = n;
      cntEl.textContent = "×" + n;
      cntEl.hidden = false;
    }
    const timeEl = lastGroupEl.querySelector(".evt-time");
    if (timeEl) timeEl.textContent = fmtTime(evt.timestamp);
    // Update the preview to the latest event's content.
    const preview = lastGroupEl.querySelector(".evt-content");
    if (preview) {
      let meta = "";
      if (evt.agent) meta += `agent: ${esc(evt.agent)} `;
      if (evt.tool)  meta += `tool: ${esc(evt.tool)} `;
      if (evt.status && evt.status !== "final") meta += `${esc(evt.status)} `;
      preview.innerHTML = meta + esc(evtContentText(evt.content)).slice(0, 200);
    }
    if ($("#evt-autoscroll") && $("#evt-autoscroll").checked) list.scrollTop = list.scrollHeight;
    return;
  }

  // ── New group row.
  // If the last row was a streaming group, finalize it (stop coalescing into
  // it) so a new type starts a fresh group.
  if (lastGroupEl) lastGroupEl.dataset.streaming = "0";

  const group = document.createElement("div");
  group.className = "evt-item evt-group";
  group.dataset.type = eType;
  group.dataset.task = eTask;
  group.dataset.streaming = isStreaming ? "1" : "0";

  const expanded = EVT_EXPANDED_TYPES.has(eType);
  group.dataset.expanded = expanded ? "1" : "0";

  const time = document.createElement("div");
  time.className = "evt-time";
  time.textContent = fmtTime(evt.timestamp);

  const type = document.createElement("div");
  type.className = "evt-type " + eType;
  type.textContent = eType.replace("_", " ");

  const content = document.createElement("div");
  content.className = "evt-content";
  let meta = "";
  if (evt.agent) meta += `<span class="evt-meta">agent: ${esc(evt.agent)}</span> `;
  if (evt.tool)  meta += `<span class="evt-meta">tool: ${esc(evt.tool)}</span> `;
  if (evt.status && evt.status !== "final") meta += `<span class="evt-meta">${esc(evt.status)}</span> `;
  content.innerHTML = meta + esc(evtContentText(evt.content)).slice(0, 200);
  if (isStreaming) content.dataset.acc = evtContentText(evt.content);

  const cnt = document.createElement("span");
  cnt.className = "evt-count";
  cnt.dataset.n = "1";
  cnt.textContent = "×1";
  cnt.hidden = true; // hidden until a 2nd event joins this group

  const chevron = document.createElement("span");
  chevron.className = "evt-chevron";
  chevron.textContent = expanded ? "▼" : "▶";
  chevron.title = "Expand / collapse";

  const details = document.createElement("div");
  details.className = "evt-group-details";
  details.hidden = !expanded;
  details.appendChild(buildEventDetail(evt));

  // Click the header (not the detail text) to toggle.
  const header = document.createElement("div");
  header.className = "evt-group-header";
  header.appendChild(time);
  header.appendChild(type);
  header.appendChild(content);
  header.appendChild(cnt);
  header.appendChild(chevron);
  header.addEventListener("click", () => {
    const isOpen = group.dataset.expanded === "1";
    group.dataset.expanded = isOpen ? "0" : "1";
    details.hidden = isOpen;
    chevron.textContent = isOpen ? "▶" : "▼";
  });

  group.appendChild(header);
  group.appendChild(details);
  list.appendChild(group);
  lastGroupEl = group;

  if ($("#evt-autoscroll") && $("#evt-autoscroll").checked) list.scrollTop = list.scrollHeight;
  while (list.children.length > 500) list.removeChild(list.firstChild);
  // If we removed the lastGroupEl (rare cap eviction), reset it.
  if (lastGroupEl && !lastGroupEl.parentElement) lastGroupEl = null;
}

// buildEventDetail creates the inner per-event row shown when a group is
// expanded. Kept lightweight so 200 grouped events don't cost 200 reflows
// (details are hidden until expand → zero layout cost while collapsed).
function buildEventDetail(evt) {
  const row = document.createElement("div");
  row.className = "evt-detail";
  let meta = "";
  if (evt.agent) meta += `<span class="evt-meta">agent: ${esc(evt.agent)}</span> `;
  if (evt.tool)  meta += `<span class="evt-meta">tool: ${esc(evt.tool)}</span> `;
  if (evt.status && evt.status !== "final") meta += `<span class="evt-meta">${esc(evt.status)}</span> `;
  row.innerHTML = `<span class="evt-detail-time">${esc(fmtTime(evt.timestamp))}</span> ` + meta + esc(evtContentText(evt.content));
  return row;
}

function showEvtBadge(n) { const b = $("#evt-badge"); if (!b) return; b.hidden = false; b.textContent = n > 99 ? "99+" : n; }
function hideEvtBadge() { const b = $("#evt-badge"); if (b) b.hidden = true; }

// ════════════════════════════════════════════════════════════════════════
