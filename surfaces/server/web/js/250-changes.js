// 250-changes.js — seeing and undoing what the agent changed.
//
// The workspace is snapshotted before every mutating tool call, and the CLI has
// always been able to show and rewind that (/log, /rollback). The same record
// was invisible here: the endpoints existed, the GUI simply never asked. It
// does now, because approving a change you cannot see is not really approving
// it.
//
// The diff is computed client-side from the two versions the server returns,
// using the same prefix/suffix-stripping walk as cli/diff.go, so a change reads
// identically whichever surface you are on.

(function () {
  const STATUS_STYLE = {
    modified: { icon: '±', color: 'var(--yellow, #eab308)' },
    created:  { icon: '+', color: 'var(--green, #22c55e)' },
    deleted:  { icon: '−', color: 'var(--red, #ef4444)' },
  };

  // maxDiffLines matches the CLI's inline cap. A full rewrite produces a diff
  // as long as the file, which helps nobody scrolling for the actual edit.
  const maxDiffLines = 400;

  let checkpointId = null;
  let selectedFile = null;

  const $ = (id) => document.getElementById(id);

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  function styleFor(status) {
    return STATUS_STYLE[status] || { icon: '·', color: 'var(--text-mute)' };
  }

  // splitLines keeps a file that ends in a newline from growing a phantom
  // trailing line, which would otherwise show up as a change in every diff.
  function splitLines(s) {
    if (!s) return [];
    const lines = s.replace(/\r\n/g, '\n').split('\n');
    if (lines.length && lines[lines.length - 1] === '') lines.pop();
    return lines;
  }

  // lineDiff strips the common prefix and suffix, then reports the changed
  // middle with a couple of lines of context on either side. Deliberately the
  // same shape as the CLI's renderer rather than a cleverer algorithm — the two
  // surfaces describing one edit differently is worse than either being terse.
  function lineDiff(before, after) {
    const b = splitLines(before);
    const a = splitLines(after);

    let pre = 0;
    while (pre < b.length && pre < a.length && b[pre] === a[pre]) pre++;

    let suf = 0;
    while (suf < b.length - pre && suf < a.length - pre &&
           b[b.length - 1 - suf] === a[a.length - 1 - suf]) suf++;

    const rows = [];
    for (let i = Math.max(0, pre - 2); i < pre; i++) rows.push([' ', b[i]]);
    for (let i = pre; i < b.length - suf; i++) rows.push(['-', b[i]]);
    for (let i = pre; i < a.length - suf; i++) rows.push(['+', a[i]]);
    for (let i = 0; i < Math.min(suf, 2); i++) rows.push([' ', a[a.length - suf + i]]);
    return rows;
  }

  function renderDiff(before, after, status) {
    const box = $('ch-diff');
    if (!box) return;

    if (!before && !after) {
      box.innerHTML = '<span style="color:var(--text-mute);">Nothing to show — the file is empty on both sides.</span>';
      return;
    }

    const rows = lineDiff(before, after);
    if (!rows.length) {
      box.innerHTML = '<span style="color:var(--text-mute);">No textual difference.</span>';
      return;
    }

    const shown = rows.slice(0, maxDiffLines);
    const tint = { '+': 'var(--green, #22c55e)', '-': 'var(--red, #ef4444)', ' ': 'var(--text-mute)' };
    const bg = { '+': 'rgba(34,197,94,0.07)', '-': 'rgba(239,68,68,0.07)', ' ': 'transparent' };

    let html = shown.map(([sign, text]) => (
      `<div style="display:flex;gap:8px;background:${bg[sign]};padding:0 4px;">
         <span style="color:${tint[sign]};width:8px;flex:none;">${sign === ' ' ? '&nbsp;' : sign}</span>
         <span style="color:${sign === ' ' ? 'var(--text-dim)' : 'var(--text)'};white-space:pre-wrap;">${esc(text) || '&nbsp;'}</span>
       </div>`
    )).join('');

    if (rows.length > shown.length) {
      html += `<div style="color:var(--text-mute);padding:6px 4px 0;">… ${rows.length - shown.length} more lines truncated</div>`;
    }
    box.innerHTML = html;

    const title = $('ch-diff-title');
    if (title) {
      const st = styleFor(status);
      title.innerHTML = `<span style="color:${st.color};">${st.icon}</span> ${esc(selectedFile || '')}`;
    }
  }

  async function loadCheckpoints() {
    const select = $('ch-checkpoint');
    if (!select) return;
    try {
      const res = await fetch('/api/checkpoints');
      const data = await res.json();
      const list = (data.checkpoints || []).slice().reverse(); // newest first
      if (!list.length) {
        select.innerHTML = '<option value="">No checkpoints yet — run the agent first</option>';
        hide('ch-files-panel', 'ch-diff-panel');
        return;
      }
      select.innerHTML = list.map((c) => {
        const when = c.time ? new Date(c.time).toLocaleString() : '';
        const label = c.label || c.tool || 'checkpoint';
        return `<option value="${c.id}">#${c.id} · ${esc(label)} — ${when}</option>`;
      }).join('');
      loadDiff(list[0].id);
    } catch (e) {
      select.innerHTML = '<option value="">Could not load checkpoints</option>';
    }
  }

  function hide(...ids) {
    ids.forEach((id) => { const el = $(id); if (el) el.style.display = 'none'; });
  }

  async function loadDiff(id) {
    checkpointId = id;
    selectedFile = null;
    hide('ch-diff-panel');
    const filesPanel = $('ch-files-panel');
    const files = $('ch-files');
    if (!files) return;

    let changes = [];
    try {
      const res = await fetch('/api/checkpoints/diff?id=' + encodeURIComponent(id));
      const data = await res.json();
      changes = data.changes || [];
    } catch (e) {
      changes = [];
    }

    if (filesPanel) filesPanel.style.display = 'block';

    const summary = $('ch-summary');
    if (summary) {
      summary.textContent = changes.length
        ? `${changes.length} file${changes.length === 1 ? '' : 's'} changed since this checkpoint`
        : 'no changes since this checkpoint';
    }

    if (!changes.length) {
      files.innerHTML = '<div style="color:var(--text-mute);font-size:12px;padding:6px;">The workspace matches this checkpoint.</div>';
      return;
    }

    files.innerHTML = changes.map((c) => {
      const st = styleFor(c.status);
      return `<div data-file="${esc(c.path)}" data-status="${esc(c.status)}"
                style="display:flex;gap:8px;align-items:center;padding:4px 6px;border-radius:4px;
                       cursor:pointer;font-size:12px;font-family:var(--font-mono);">
                <span style="color:${st.color};width:10px;flex:none;">${st.icon}</span>
                <span style="color:var(--text-mute);width:64px;flex:none;">${esc(c.status)}</span>
                <span style="color:var(--text);flex:1;overflow:hidden;text-overflow:ellipsis;
                      white-space:nowrap;">${esc(c.path)}</span>
              </div>`;
    }).join('');
  }

  async function openFile(path, status) {
    selectedFile = path;
    const panel = $('ch-diff-panel');
    if (panel) panel.style.display = 'block';
    const box = $('ch-diff');
    if (box) box.innerHTML = '<span style="color:var(--text-mute);">Loading…</span>';

    // Highlight the active row.
    const files = $('ch-files');
    if (files) {
      files.querySelectorAll('[data-file]').forEach((row) => {
        row.style.background = row.dataset.file === path ? 'var(--bg-panel)' : 'transparent';
      });
    }

    try {
      const res = await fetch('/api/checkpoints/diff?id=' + encodeURIComponent(checkpointId) +
                              '&file=' + encodeURIComponent(path));
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
      renderDiff(data.before || '', data.after || '', status);
    } catch (e) {
      if (box) box.innerHTML = `<span style="color:var(--red, #ef4444);">Could not load diff: ${esc(e.message)}</span>`;
    }
  }

  // Restoring is destructive, so it asks first and then reloads the listing —
  // a stale file list after an undo is how someone restores the same thing
  // twice.
  async function rollback(body, confirmText) {
    if (!window.confirm(confirmText)) return;
    try {
      const res = await fetch('/api/rollback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
      if (typeof toast === 'function') toast('success', 'Restored from checkpoint #' + body.id);
      await loadDiff(checkpointId);
      if (typeof loadFileTree === 'function') loadFileTree();
    } catch (e) {
      if (typeof toast === 'function') toast('error', 'Rollback failed: ' + e.message);
      else window.alert('Rollback failed: ' + e.message);
    }
  }

  function wire() {
    const on = (id, ev, fn) => { const el = $(id); if (el) el.addEventListener(ev, fn); };

    on('ch-refresh', 'click', loadCheckpoints);
    on('ch-checkpoint', 'change', (e) => loadDiff(Number(e.target.value)));

    on('ch-rollback-all', 'click', () => {
      if (checkpointId === null) return;
      rollback({ id: checkpointId },
        `Restore the entire workspace to checkpoint #${checkpointId}?\n\n` +
        `A snapshot is taken first, so this undo can itself be undone.`);
    });

    on('ch-rollback-file', 'click', () => {
      if (checkpointId === null || !selectedFile) return;
      rollback({ id: checkpointId, file: selectedFile },
        `Restore "${selectedFile}" to its state at checkpoint #${checkpointId}?`);
    });

    const files = $('ch-files');
    if (files) {
      files.addEventListener('click', (e) => {
        const row = e.target.closest('[data-file]');
        if (row) openFile(row.dataset.file, row.dataset.status);
      });
    }
  }

  // The page is injected when its tab is first shown, so wiring waits for the
  // element to exist rather than for DOMContentLoaded.
  function init() {
    if (!$('ch-checkpoint')) return false;
    wire();
    loadCheckpoints();
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
