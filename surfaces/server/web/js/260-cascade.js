// 260-cascade.js — the cost-savings proof surface.
//
// The cascade's whole claim is that most questions never reach a paid model.
// That claim was measurable but not visible: the kernel has kept per-rung
// counters and a decision log all along, and /api/cascade has served them, with
// nothing on either surface reading it. A saving nobody can see is one nobody
// trusts.
//
// Counters come from the server's lifetime stats rather than from the recent
// log, which is capped — deriving the headline from the log would quietly
// under-report as soon as the cap was reached.

(function () {
  const RUNG_COLOR = {
    deterministic: 'var(--green, #22c55e)',
    cache:         'var(--cyan, #26c6da)',
    graph:         'var(--blue, #3b82f6)',
    recall:        'var(--purple, #ba68c8)',
    llm:           'var(--text-mute)',
  };

  const $id = (id) => document.getElementById(id);

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  const colorFor = (name) => RUNG_COLOR[name] || 'var(--text-dim)';

  function renderHeadline(stats) {
    // A rung named "llm" counts escalations, not savings; everything else
    // answered without a model call.
    let avoided = 0;
    let escalated = 0;
    stats.forEach((s) => {
      if (s.name === 'llm') escalated += s.answered;
      else avoided += s.answered;
    });
    const total = avoided + escalated;

    const set = (id, text) => { const el = $id(id); if (el) el.textContent = text; };
    set('cs-avoided', String(avoided));
    set('cs-total', String(total));
    set('cs-rate', total ? Math.round((avoided / total) * 100) + '%' : '–');
  }

  function renderRungs(stats) {
    const box = $id('cs-rungs');
    if (!box) return;
    if (!stats.length) {
      box.innerHTML = '<div style="color:var(--text-mute);font-size:12px;">No cascade activity recorded yet.</div>';
      return;
    }
    const peak = Math.max(1, ...stats.map((s) => s.answered));
    box.innerHTML = stats.map((s) => {
      const width = Math.round((s.answered / peak) * 100);
      const color = colorFor(s.name);
      // A retry is the negative label: the user re-asked, so that answer did
      // not land. Worth showing next to the win count, not buried.
      const retries = s.retried
        ? `<span style="color:var(--yellow, #eab308);" title="answers the user immediately re-asked">${s.retried} re-asked</span>`
        : '';
      return `<div style="display:flex;gap:10px;align-items:center;font-size:12px;font-family:var(--font-mono);">
                <span style="color:${color};width:104px;flex:none;">${esc(s.name)}</span>
                <span style="flex:1;background:var(--bg-panel);border-radius:3px;height:8px;overflow:hidden;">
                  <span style="display:block;height:100%;width:${width}%;background:${color};"></span>
                </span>
                <span style="color:var(--text);width:52px;text-align:right;flex:none;">${s.answered}</span>
                <span style="color:var(--text-mute);width:96px;text-align:right;flex:none;"
                      title="current answer threshold">θ ${Number(s.threshold).toFixed(2)}</span>
                <span style="width:88px;text-align:right;flex:none;">${retries}</span>
              </div>`;
    }).join('');
  }

  function renderLog(log) {
    const box = $id('cs-log');
    if (!box) return;
    if (!log.length) {
      box.innerHTML = '<div style="color:var(--text-mute);font-size:12px;">Nothing recorded yet — ask the agent something.</div>';
      return;
    }
    // Newest first: the server appends, and the interesting decision is the
    // one that just happened.
    box.innerHTML = log.slice().reverse().map((e) => {
      const color = colorFor(e.rung_name);
      const when = e.time ? new Date(e.time).toLocaleTimeString() : '';
      const verdict = e.answered ? 'local' : 'escalated';
      const flag = e.retried
        ? '<span style="color:var(--yellow, #eab308);" title="the user re-asked this — the local answer did not land">re-asked</span>'
        : '';
      return `<div style="display:flex;gap:10px;align-items:center;padding:3px 6px;border-radius:4px;
                    font-size:11px;font-family:var(--font-mono);">
                <span style="color:var(--text-mute);width:64px;flex:none;">${when}</span>
                <span style="color:${color};width:104px;flex:none;">${esc(e.rung_name || '?')}</span>
                <span style="color:${e.answered ? 'var(--green, #22c55e)' : 'var(--text-mute)'};
                      width:66px;flex:none;">${verdict}</span>
                <span style="color:var(--text);flex:1;overflow:hidden;text-overflow:ellipsis;
                      white-space:nowrap;">${esc(String(e.query || '').slice(0, 110))}</span>
                <span style="color:var(--text-mute);width:56px;text-align:right;flex:none;">${e.latency_ms}ms</span>
                <span style="width:64px;text-align:right;flex:none;">${flag}</span>
              </div>`;
    }).join('');
  }

  async function load() {
    try {
      const res = await fetch('/api/cascade');
      const data = await res.json();
      const stats = data.stats || [];
      renderHeadline(stats);
      renderRungs(stats);
      renderLog(data.log || []);
    } catch (e) {
      const box = $id('cs-rungs');
      if (box) box.innerHTML = `<div style="color:var(--red, #ef4444);font-size:12px;">Could not load cascade telemetry: ${esc(e.message)}</div>`;
    }
  }

  function init() {
    if (!$id('cs-rungs')) return false;
    const btn = $id('cs-refresh');
    if (btn) btn.addEventListener('click', load);
    load();
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();

/* ── Consensus & debate ───────────────────────────────────────────────
 *
 * Restored deliberately. An earlier pass removed the consensus panel on the
 * grounds that it read IDLE with every value a dash — which was true, and was
 * the wrong conclusion: it read IDLE because the kernel computed the verdict
 * and threw everything but the answer away. Multi-model consensus and the
 * debate that settles a conflict are the reason this project pays for more
 * than one model, so the fix was to emit the record, not to delete the panel.
 *
 * The consensus event now carries how the verdict was reached, whether an
 * exchange ran, and its transcript.
 */
(function () {
  const csHistory = [];

  function set(id, v) {
    const el = document.getElementById(id);
    if (el) el.textContent = v;
  }

  function renderConsensus(evt) {
    const d = (evt && typeof evt.content === "object") ? evt.content : {};
    const conflict = evt.status === "conflict";

    set("cs-model-count", d.models || 0);
    set("cs-method", d.method || "—");
    set("cs-conflict", conflict ? "yes" : "no");
    set("cs-debated", d.debated ? "yes" : "no");

    const live = document.getElementById("consensus-live");
    if (live) {
      live.textContent = conflict ? "● CONFLICT" : "● SETTLED";
      live.style.color = conflict ? "var(--red, #ef4444)" : "var(--green, #22c55e)";
    }

    // The transcript is the point. Without it "debated: yes" is a claim.
    const wrap = document.getElementById("cs-transcript-wrap");
    const pre = document.getElementById("cs-transcript");
    if (wrap && pre) {
      if (d.transcript) { pre.textContent = d.transcript; wrap.hidden = false; }
      else { wrap.hidden = true; }
    }

    csHistory.push({
      at: new Date(evt.timestamp || Date.now()),
      method: d.method || "—",
      conflict,
      debated: !!d.debated,
      who: Array.isArray(d.contributors) ? d.contributors : [],
    });
    renderCsHistory();
  }

  function renderCsHistory() {
    const el = document.getElementById("cs-history");
    if (!el) return;
    if (!csHistory.length) return;
    el.innerHTML = csHistory.slice(-15).reverse().map((h) => `
      <div style="border-left:3px solid ${h.conflict ? "var(--red,#ef4444)" : "var(--green,#22c55e)"}; padding-left:10px; margin-bottom:10px;">
        <div style="display:flex; justify-content:space-between; font-size:11px;">
          <span style="color:var(--text)">decided by <b>${h.method}</b>${h.debated ? " after one exchange" : ""}</span>
          <span style="color:var(--text-mute)">${h.at.toLocaleTimeString()}</span>
        </div>
        ${h.who.length ? `<div style="font-size:10px; color:var(--text-mute); margin-top:2px;">${h.who.join(" · ")}</div>` : ""}
      </div>`).join("");
  }

  // Ride the existing event bus rather than opening a second stream.
  EventBus.on("consensus", (evt) => {
    try { renderConsensus(evt); } catch (e) { console.error("consensus panel:", e); }
  });
})();
