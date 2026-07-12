/* 20-approvals.js — extracted from app.js (lines 335-432) */
// PERMISSION POPUP
// When the orchestrator is about to run a dangerous tool call, the server
// blocks and emits an "approval" SSE event with status "request". We pop up
// a modal and POST the user's decision back to /api/approvals/decide, which
// unblocks the agent. A "decided" event closes the popup.
// ════════════════════════════════════════════════════════════════════════
let pendingApprovalId = null;

function handleApprovalEvent(data) {
  const status = data.status || (data.content && data.content.status);
  const payload = data.content || data;
  if (status === "decided") {
    closeApprovalPopup(payload.decision || "");
    return;
  }
  // Show the popup only for resolvable requests. A request without an id
  // cannot be answered — /api/approvals/decide requires an id, and without
  // one submitApproval() no-ops, leaving an unanswerable dialog on screen.
  if (payload.id) {
    showApprovalPopup(payload);
  }
}

function showApprovalPopup(req) {
  pendingApprovalId = req.id;
  pendingApprovalData = req;
  const btn = $("#reopen-approval-btn");
  if (btn) btn.style.display = "flex";
  const risk = (req.risk || "medium").toLowerCase();
  $("#perm-tool").textContent = req.tool || "unknown";
  $("#perm-summary").textContent = req.summary || "";
  $("#perm-preview").textContent = req.preview || "";
  const riskEl = $("#perm-risk");
  riskEl.textContent = risk + " risk";
  riskEl.className = "perm-risk " + risk;
  $("#perm-hint").textContent = "The agent is waiting for your decision…";
  const fb = $("#perm-feedback");
  if (fb) fb.value = "";
  ["#perm-allow-once", "#perm-allow-session", "#perm-deny"].forEach((s) => { const b = $(s); if (b) b.disabled = false; });
  $("#perm-overlay").classList.add("active");
}

function closeApprovalPopup(decision) {
  pendingApprovalId = null;
  pendingApprovalData = null;
  const btn = $("#reopen-approval-btn");
  if (btn) btn.style.display = "none";
  $("#perm-overlay").classList.remove("active");
  if (decision) toast("info", "Permission: " + decision);
}

async function submitApproval(decision) {
  if (!pendingApprovalId) return;
  const id = pendingApprovalId;
  // Collect optional in-between feedback the user typed. It is sent to the
  // backend and surfaced back to the agent through the tool-result channel so
  // the agent adapts (e.g. deny + "use /tmp instead" → the agent retries with
  // /tmp). Empty feedback is fine for a plain allow/deny.
  const feedback = $("#perm-feedback")?.value?.trim() || "";
  ["#perm-allow-once", "#perm-allow-session", "#perm-deny"].forEach((s) => { const b = $(s); if (b) b.disabled = true; });
  $("#perm-hint").textContent = "Sending decision…";
  try {
    const res = await fetch(API + "/api/approvals/decide", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: id, decision: decision, feedback: feedback }),
    });
    const data = await res.json();
    if (!res.ok || data.error) {
      toast("error", "Approval failed: " + (data.error || res.status));
      $("#perm-hint").textContent = "Failed — try again.";
      ["#perm-allow-once", "#perm-allow-session", "#perm-deny"].forEach((s) => { const b = $(s); if (b) b.disabled = false; });
      return;
    }
    // The server's OnDecision hook will emit a "decided" event that closes
    // the popup; close optimistically to feel snappy.
    closeApprovalPopup(decision);
  } catch (err) {
    toast("error", "Approval request failed: " + err.message);
    $("#perm-hint").textContent = "Network error — try again.";
    ["#perm-allow-once", "#perm-allow-session", "#perm-deny"].forEach((s) => { const b = $(s); if (b) b.disabled = false; });
  }
}

// On reconnect, poll for any approval that arrived while the SSE was down.
async function pollPendingApprovals() {
  if (pendingApprovalId) return;
  try {
    const res = await fetch(API + "/api/approvals");
    const data = await res.json();
    if (data.approvals && data.approvals.length > 0) {
      const a = data.approvals[0];
      showApprovalPopup(a);
    }
  } catch (e) { /* ignore */ }
}

// ════════════════════════════════════════════════════════════════════════
