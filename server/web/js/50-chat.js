/* 50-chat.js — extracted from app.js (lines 682-831) */
// CHAT
// ════════════════════════════════════════════════════════════════════════
let pendingChat = false;

function formatErrorForChat(errStr, force = false) {
  const str = String(errStr);
  const isError = force || str.startsWith("Error: API error") || str.includes("RESOURCE_EXHAUSTED") || str.includes('"error": {');
  if (!isError) return { formatted: false, text: str };

  let title = "An error occurred";
  if (str.includes("429") || str.includes("quota") || str.includes("RESOURCE_EXHAUSTED")) {
    title = "Usage quota exceeded";
  } else if (str.includes("401") || str.includes("auth")) {
    title = "Authentication failed";
  } else if (str.includes("timeout") || str.includes("deadline")) {
    title = "Request timed out";
  } else {
    title = "Error executing request";
  }
  return {
    formatted: true,
    html: `<div style="color:var(--red);"><strong>${title}</strong></div><br><details><summary style="cursor:pointer;color:var(--text-dim);">View Details</summary><div style="margin-top:8px;white-space:pre-wrap;background:var(--bg-deep);padding:10px;border-radius:6px;color:var(--text-mute);font-family:var(--font-mono);font-size:11px;border:1px solid var(--border);">${esc(str)}</div></details>`
  };
}

async function sendChat() {
  const text = $("#chat-text").value.trim();
  if (!text || pendingChat) return;

  // Snapshot the selected attachments and clear the tray immediately so the
  // user sees them “consumed” by the outgoing message. They are resolved
  // server-side into a markdown block prepended to the query.
  const attachments = chatAttachments.slice();
  if (attachments.length) { chatAttachments = []; renderAttachTray(); }

  appendMsg("user", text);
  $("#chat-text").value = "";
  $("#chat-text").style.height = "auto";
  $("#chat-send").disabled = true;

  // If a project is active, surface that its context is being injected so
  // the user can see the project knowledge is in scope for this query.
  if (activeProjectId) {
    const parts = ["📁 Project context active"];
    if (activeProjectName) parts.push(activeProjectName);
    parts.push(activeContextLen > 0 ? `${activeContextLen.toLocaleString()} chars injected into prompt` : "no context saved yet");
    appendSystemNote(parts.join(" · "));
  }

  const loadingEl = appendMsg("assistant", "Orchestrating workflow...", true);
  pendingChat = true;
  pendingChatAnswer = "";
  window.currentStreamingMsgEl = null; // Reset streaming element

  // (P2) The previous code snapshotted the whole workspace tree BEFORE the
  // run (a full /api/files/list fetch) and again after, just to diff
  // mtimes. The SSE tool_execution handler already records file ops live,
  // so the pre-snapshot was redundant network cost on every send. We now do
  // a single post-run refresh; detectFileChanges() does its own fetch.

  try {
    const res = await fetch(API + "/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ 
        query: text, 
        project: activeProjectId || "", 
        attachments
      }),
    });
    const data = await res.json();
    if (data.error) {
      const formatted = formatErrorForChat(data.error, true);
      finalizeAssistantMessage(loadingEl, formatted.formatted ? formatted.html : formatted.text, true, formatted.formatted);
      toast("error", "Request failed");
    } else if (pendingChat) {
      if (window.currentStreamingMsgEl) {
        // If we were streaming, the text is already in the UI. Just finalize it.
        window.currentStreamingMsgEl.classList.remove("loading");
        window.currentStreamingMsgEl = null;
      } else {
        pendingChatAnswer = String(data.output || "");
        finalizeAssistantMessage(loadingEl, data.output || "(empty response)", false, false);
      }
    }
  } catch (err) {
    const formatted = formatErrorForChat(err.message, true);
    finalizeAssistantMessage(loadingEl, formatted.formatted ? formatted.html : formatted.text, true, formatted.formatted);
    toast("error", "Request failed");
  } finally {
    pendingChat = null;
    $("#chat-send").disabled = false;
    $("#chat-text").focus();
    // (P2) Single post-run diff (no before-snapshot): surface any file
    // modifications the agent made that the SSE tool events didn't catch.
    detectFileChanges(null);
    refreshFileTree();
    // Persist this exchange into the active project's context.md so the
    // project accumulates knowledge across queries. This is now done
    // automatically by the backend (and rewritten using the local LLM).
    // The UI relies on SSE 'summary_updated' to refresh the context view.
  }
}

function appendMsg(role, text, loading, isError, isHtml = false) {
  const container = $("#chat-messages");
  const empty = container.querySelector(".chat-empty");
  if (empty) empty.remove();

  const msg = document.createElement("div");
  msg.className = "msg " + role;
  if (loading) msg.classList.add("loading");
  if (isError) msg.classList.add("error");

  const avatar = document.createElement("div");
  avatar.className = "msg-avatar";
  avatar.textContent = role === "user" ? "U" : "H";

  const body = document.createElement("div");
  body.className = "msg-body";
  const roleEl = document.createElement("div");
  roleEl.className = "msg-role";
  roleEl.textContent = role === "user" ? "USER" : "DARKCODE-GO";
  const textEl = document.createElement("div");
  textEl.className = "msg-text";
  if (loading) {
      textEl.innerHTML = `
          <div style="display: flex; align-items: center; gap: 12px; margin-bottom: 12px;">
              <span style="font-weight: 500; letter-spacing: 0.5px; opacity: 0.9;">${isHtml ? text : renderMarkdown(text || "")}</span>
              <button class="stop-btn" type="button" title="Stop Execution">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>
              </button>
          </div>
          <div class="inline-exec-timeline custom-scrollbar msg-exec-live" hidden></div>
      `;
      // Wire the stop button via addEventListener (F4) instead of an inline
      // onclick="cancelChatExecution()" attribute baked into innerHTML —
      // avoids relying on a global + is CSP-safe + survives re-renders.
      const stopBtn = textEl.querySelector(".stop-btn");
      if (stopBtn) stopBtn.addEventListener("click", cancelChatExecution);
  } else {
      textEl.innerHTML = isHtml ? text : renderMarkdown(text || "");
      // Store the raw text for edit-and-resend (user messages only).
      if (role === "user" && !isHtml) {
          textEl.dataset.raw = text || "";
      }
  }
  body.appendChild(roleEl);
  body.appendChild(textEl);

  // Edit-and-resend button below user messages (ChatGPT-style). Clicking it
  // loads the original text back into the composer and trims the conversation
  // from this message onward, so the user can edit + re-send.
  if (role === "user" && !loading && !isHtml) {
      const actions = document.createElement("div");
      actions.className = "msg-actions";
      const editBtn = document.createElement("button");
      editBtn.className = "msg-edit-btn";
      editBtn.type = "button";
      editBtn.innerHTML = '✎ Edit';
      editBtn.title = "Edit message and resend";
      editBtn.addEventListener("click", () => editUserMessage(msg));
      actions.appendChild(editBtn);
      body.appendChild(actions);
  }

  msg.appendChild(avatar);
  msg.appendChild(body);
  container.appendChild(msg);
  container.scrollTop = container.scrollHeight;
  return msg;
}

// editUserMessage loads a user message's raw text back into the composer and
// removes that message + everything after it from the chat stream, so the
// user can edit and resend (standard ChatGPT edit-and-resend UX).
function editUserMessage(msgEl) {
  if (!msgEl) return;
  const textEl = msgEl.querySelector(".msg-text");
  const raw = textEl?.dataset?.raw || textEl?.textContent || "";
  const composer = $("#chat-text");
  if (composer) {
      composer.value = raw;
      composer.style.height = "auto";
      composer.style.height = composer.scrollHeight + "px";
      composer.focus();
  }
  // Remove this message and all siblings after it.
  let next = msgEl.nextElementSibling;
  while (next) {
      const after = next.nextElementSibling;
      next.remove();
      next = after;
  }
  msgEl.remove();
}

// finalizeAssistantMessage transforms a loading assistant message into its
// final state, preserving the live execution trace that accumulated in its
// .inline-exec-timeline during the run (instead of the old remove()+recreate
// which discarded the trace and caused a flash). Idempotent via data-finalized.
// Exposed on window.DC so the V2 SSE handler can call it as a safety net.
function finalizeAssistantMessage(msgEl, output, isError, isHtml) {
  if (!msgEl || msgEl.dataset.finalized === "1") return;
  msgEl.dataset.finalized = "1";

  // Remove the stop button (no longer running).
  const stopBtn = msgEl.querySelector(".stop-btn");
  if (stopBtn) stopBtn.remove();

  // Swap the loading text for the final output.
  const textEl = msgEl.querySelector(".msg-text");
  if (textEl) {
      if (isError) {
          msgEl.classList.add("error");
          textEl.innerHTML = isHtml ? output : esc(output || "");
      } else {
          textEl.innerHTML = isHtml ? output : renderMarkdown(output || "(empty response)");
          if (window.mermaid) {
            try { mermaid.init(undefined, textEl.querySelectorAll('.mermaid')); } catch (e) {}
          }
      }
  }
  msgEl.classList.remove("loading");

  // Convert the live .inline-exec-timeline into a collapsible
  // .msg-exec-details (collapsed by default after completion). If the
  // timeline has no content, remove it entirely.
  const liveTimeline = msgEl.querySelector(".inline-exec-timeline");
  if (liveTimeline) {
      const hasContent = liveTimeline.children.length > 0;
      if (!hasContent) {
          liveTimeline.remove();
      } else {
          liveTimeline.classList.remove("msg-exec-live");
          liveTimeline.hidden = false;
          const wrap = document.createElement("div");
          wrap.className = "msg-exec-details";
          const toggle = document.createElement("div");
          toggle.className = "msg-exec-toggle";
          toggle.innerHTML = '<span class="msg-exec-chevron">▶</span> Execution Details <span class="msg-exec-count">(' + liveTimeline.children.length + ')</span>';
          const bodyDiv = document.createElement("div");
          bodyDiv.className = "msg-exec-body";
          bodyDiv.style.display = "none";
          bodyDiv.appendChild(liveTimeline);
          toggle.addEventListener("click", () => {
              const open = bodyDiv.style.display !== "none";
              bodyDiv.style.display = open ? "none" : "block";
              const ch = toggle.querySelector(".msg-exec-chevron");
              if (ch) ch.textContent = open ? "▶" : "▼";
          });
          wrap.appendChild(toggle);
          wrap.appendChild(bodyDiv);
          // Append the collapsible wrapper where the timeline was.
          if (textEl && textEl.parentElement) {
              textEl.parentElement.appendChild(wrap);
          }
      }
  }

  // Scroll the final message into view.
  const container = $("#chat-messages");
  if (container) container.scrollTop = container.scrollHeight;
}

// Expose finalizeAssistantMessage for the V2 SSE handler (safety net when
// final_output / chat_response arrives before the fetch resolves).
window.DC = window.DC || {};
window.DC.finalizeAssistantMessage = finalizeAssistantMessage;

// ════════════════════════════════════════════════════════════════════════
