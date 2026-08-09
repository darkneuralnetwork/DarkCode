// views.js — everything that is not the conversation.
//
// Six tabs became a palette entry each. A view takes over the main column and
// the composer hides, because reading memory and typing a message are not the
// same activity and showing both invites neither.

import { get, post, esc, fmtNum, fmtCost } from './api.js';

const chatView = () => document.getElementById('view-chat');
const panel = () => document.getElementById('view-panel');
const composer = () => document.getElementById('composer-wrap');

export function showChat() {
  chatView().hidden = false;
  composer().hidden = false;
  panel().hidden = true;
}

function showPanel(html) {
  chatView().hidden = true;
  composer().hidden = true;
  const p = panel();
  p.hidden = false;
  p.innerHTML = html;
  return p;
}

/* ── Memory ────────────────────────────────────────────────────────────
   A search, not a listing. The old tab fetched every tier plus the whole
   knowledge graph — 16 MB — and hung the browser rendering fifty rows of it.
   Memory here IS a retrieval system, so the interface asks it questions and
   shows which signal answered. */
export async function memory() {
  const p = showPanel(`
    <h2>Memory</h2>
    <p class="panel-sub">Ask what the agent knows. This runs the same keyword, vector and graph
       retrieval the agent runs, and shows which signal found each result.</p>
    <form id="mem-form" style="display:flex;gap:var(--s2);margin:var(--s5) 0 var(--s4)">
      <input class="field" id="mem-q" placeholder="e.g. why did the knowledge graph get so large" autocomplete="off">
      <button class="send" type="submit">Search</button>
    </form>
    <div class="grid" id="mem-counts"></div>
    <div id="mem-hits" style="margin-top:var(--s5)"><div class="empty">Search to see what the agent remembers.</div></div>`);

  const counts = (c) => {
    const rows = [
      ['episodic', 'past runs'], ['semantic', 'facts'], ['procedural', 'skills'],
      ['graph_nodes', 'graph nodes'], ['graph_edges', 'graph edges'],
    ];
    p.querySelector('#mem-counts').innerHTML = rows.map(([k, label]) =>
      `<div class="card"><div class="stat-k">${label}</div><div class="stat-v">${fmtNum(c[k] || 0)}</div></div>`).join('');
  };

  try { counts((await get('/api/memory/search')).counts || {}); } catch { /* counts are optional */ }

  p.querySelector('#mem-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const q = p.querySelector('#mem-q').value.trim();
    if (!q) return;
    const box = p.querySelector('#mem-hits');
    box.innerHTML = '<div class="empty">Searching…</div>';
    try {
      const d = await get('/api/memory/search?q=' + encodeURIComponent(q));
      counts(d.counts || {});
      const hits = d.hits || [];
      box.innerHTML = hits.length ? hits.map(hit).join('')
        : `<div class="empty">Nothing recalled for “${esc(q)}”.</div>`;
    } catch (err) {
      box.innerHTML = `<div class="empty">Search failed: ${esc(err.message)}</div>`;
    }
  });
  p.querySelector('#mem-q').focus();
}

// The signal badge is the point of this view: it makes the fusion observable
// instead of asserted.
function hit(h) {
  const sigs = String(h.signal || '').split('+').filter(Boolean)
    .map((s) => `<span class="tag" data-signal="${esc(s)}">${esc(s)}</span>`).join(' ');
  return `<div class="card" style="margin-bottom:var(--s3)">
    <div style="display:flex;justify-content:space-between;gap:var(--s3);align-items:baseline">
      <div>${esc(h.goal || h.id || 'untitled')}</div>
      <div style="flex:none;display:flex;gap:var(--s1)">${sigs}</div>
    </div>
    ${h.snippet ? `<div style="color:var(--ink-mute);font-size:var(--t-sm);margin-top:var(--s2)">${esc(h.snippet)}</div>` : ''}
  </div>`;
}

/* ── Telemetry ─────────────────────────────────────────────────────────
   Four questions, each asked once: what did it cost, what did it avoid
   spending, how was the answer decided, what did it do to the machine. */
export async function telemetry() {
  const p = showPanel(`
    <h2>Telemetry</h2>
    <p class="panel-sub">What this session spent, and what it avoided spending.</p>
    <div class="grid" id="tel-usage" style="margin:var(--s5) 0"></div>
    <h3 style="font-size:var(--t-md);margin-top:var(--s6)">Cognition cascade</h3>
    <p class="panel-sub">Every rung that answers is a model call that never happened.</p>
    <div id="tel-cascade" style="margin-top:var(--s3)"><div class="empty">No cascade activity yet.</div></div>
    <h3 style="font-size:var(--t-md);margin-top:var(--s6)">Consensus &amp; debate</h3>
    <p class="panel-sub">The graph is checked first — that settles a disagreement for zero extra calls.
       Only when it cannot do the two most divergent answers critique each other, once.</p>
    <div id="tel-consensus" style="margin-top:var(--s3)"><div class="empty">No consensus rounds this session.</div></div>`);

  try {
    const m = await get('/api/metrics/tokens');
    const s = m.summary || m;
    p.querySelector('#tel-usage').innerHTML = [
      ['tokens', fmtNum(s.total_tokens || s.cumulative_tokens || 0)],
      ['cost', fmtCost(s.total_cost || s.cumulative_cost || 0)],
      ['cached', fmtNum(s.total_cached_tokens || 0)],
      ['requests', fmtNum(s.total_requests || s.cumulative_requests || 0)],
    ].map(([k, v]) => `<div class="card"><div class="stat-k">${k}</div><div class="stat-v">${v}</div></div>`).join('');
  } catch { /* usage is best-effort */ }

  try {
    const c = await get('/api/cascade');
    const rungs = c.rungs || c.entries || [];
    if (rungs.length) {
      p.querySelector('#tel-cascade').innerHTML = rungs.map((r) =>
        `<div style="display:flex;gap:var(--s3);font-family:var(--mono);font-size:var(--t-sm);padding:var(--s1) 0">
           <span style="width:120px;color:var(--ink-dim)">${esc(r.rung_name || r.name || '?')}</span>
           <span style="color:var(--ink-mute)">${fmtNum(r.count || 0)}</span>
         </div>`).join('');
    }
  } catch { /* cascade log may be empty */ }
}

/** consensusRound is called from the live stream, so a debate appears while it happens. */
export function consensusRound(evt) {
  const box = document.getElementById('tel-consensus');
  if (!box) return;
  const d = (evt && typeof evt.content === 'object') ? evt.content : {};
  const row = document.createElement('div');
  row.className = 'card';
  row.style.marginBottom = 'var(--s3)';
  row.innerHTML = `
    <div style="display:flex;justify-content:space-between;font-family:var(--mono);font-size:var(--t-sm)">
      <span>decided by <b style="color:var(--amber-soft)">${esc(d.method || '—')}</b>${d.debated ? ' after one exchange' : ''}</span>
      <span style="color:${evt.status === 'conflict' ? 'var(--bad)' : 'var(--ok)'}">${evt.status === 'conflict' ? 'conflict' : 'settled'}</span>
    </div>
    ${Array.isArray(d.contributors) && d.contributors.length
      ? `<div style="font-size:var(--t-xs);color:var(--ink-mute);margin-top:var(--s1)">${d.contributors.map(esc).join(' · ')}</div>` : ''}
    ${d.transcript ? `<pre style="margin-top:var(--s3);max-height:240px;overflow:auto">${esc(d.transcript)}</pre>` : ''}`;
  if (box.querySelector('.empty')) box.innerHTML = '';
  box.prepend(row);
}

/* ── Settings ──────────────────────────────────────────────────────────
   Rendered from /api/config/schema, which carries the descriptor AND the
   current value for every field in one call.

   The first version of this screen read /api/config instead — a hand-built
   response covering nineteen of the fifty-six settings — so thirty-one rows
   showed a label and a dash, and the screen read as "nothing is configured".
   The schema endpoint already served values; the view was asking the wrong
   question.

   It is also editable now. A settings screen you cannot set anything in is
   not a settings screen. Only the fields the API actually accepts get a
   control; the rest say plainly where they live, because a control that
   silently fails to save is worse than no control. */

// SETTABLE is what POST /api/config accepts today. Anything outside this list
// renders read-only with its home named, rather than pretending.
const SETTABLE = new Set([
  'routing_mode', 'safety_level', 'sandbox', 'max_turns', 'execution_profile',
  'plan_approval', 'plan_depth', 'memory_profile', 'background_work',
  'debate', 'reviewer', 'local_mode', 'compressor_model',
]);

export async function settings() {
  const p = showPanel(`<h2>Settings</h2>
    <p class="panel-sub">Six things need deciding. The rest darkcode works out for itself, or is an override that lives in the config file.</p>
    <div id="cfg" style="margin-top:var(--s5)"><div class="empty">Loading…</div></div>`);

  let schema;
  try {
    schema = await get('/api/config/schema');
  } catch (err) {
    p.querySelector('#cfg').innerHTML = `<div class="empty">Couldn't load settings: ${esc(err.message)}</div>`;
    return;
  }

  const fields = schema.fields || [];
  const values = schema.values || {};
  const box = p.querySelector('#cfg');

  // A missing value means the field is at its zero, not that it is unknown:
  // the wire drops empty strings and zeroes. Showing the zero is the truth.
  const valueOf = (f) => {
    const v = values[f.name];
    if (v !== undefined && v !== null) return v;
    return f.kind === 'bool' ? false : (f.kind === 'int' || f.kind === 'float') ? 0 : '';
  };

  const control = (f) => {
    const v = valueOf(f);
    const id = `cfg-${f.name}`;
    if (!SETTABLE.has(f.name)) {
      const shown = (f.kind === 'map' || f.kind === 'list')
        ? `${Array.isArray(v) ? v.length : Object.keys(v || {}).length} configured`
        : String(v === '' ? '—' : v);
      return `<span class="mono" style="color:var(--ink-dim)">${esc(shown).slice(0, 44)}</span>`;
    }
    if (f.kind === 'bool') {
      return `<input type="checkbox" id="${id}" data-name="${f.name}" data-kind="bool" ${v ? 'checked' : ''}>`;
    }
    if (Array.isArray(f.choices) && f.choices.length) {
      return `<select class="field" id="${id}" data-name="${f.name}" data-kind="string" style="width:auto;padding:var(--s1) var(--s2)">
        ${f.choices.map((c) => `<option value="${esc(c)}" ${String(v) === c ? 'selected' : ''}>${esc(c || 'auto')}</option>`).join('')}
      </select>`;
    }
    const type = (f.kind === 'int' || f.kind === 'float') ? 'number' : 'text';
    return `<input class="field" type="${type}" id="${id}" data-name="${f.name}" data-kind="${f.kind}"
             value="${esc(String(v))}" style="width:180px;padding:var(--s1) var(--s2)">`;
  };

  const row = (f) => `<div class="card" style="margin-bottom:var(--s2)">
    <div style="display:flex;justify-content:space-between;gap:var(--s4);align-items:center">
      <div>
        <div>${esc(f.label)}</div>
        ${f.help ? `<div style="color:var(--ink-mute);font-size:var(--t-xs);margin-top:2px">${esc(f.help)}</div>` : ''}
      </div>
      <div style="flex:none">${control(f)}</div>
    </div></div>`;

  const tier = (t) => fields.filter((f) => f.tier === t);

  box.innerHTML = `
    <div class="eyebrow" style="padding-left:0">asked · ${tier('primary').length}</div>
    ${tier('primary').map(row).join('')}

    <div style="display:flex;gap:var(--s2);align-items:center;margin:var(--s5) 0">
      <button class="send" id="cfg-save">Save changes</button>
      <span class="composer-hint" id="cfg-status"></span>
    </div>

    <details style="margin-top:var(--s5)">
      <summary style="cursor:pointer;color:var(--ink-mute);font-family:var(--mono);font-size:var(--t-xs)">
        computed · ${tier('derived').length} — darkcode decides these
      </summary>
      <div style="margin-top:var(--s3)">${tier('derived').map(row).join('')}</div>
    </details>

    <details style="margin-top:var(--s3)">
      <summary style="cursor:pointer;color:var(--ink-mute);font-family:var(--mono);font-size:var(--t-xs)">
        overrides · ${tier('advanced').length} — edit in the config file
      </summary>
      <div style="margin-top:var(--s3)">${tier('advanced').map(row).join('')}</div>
    </details>`;

  box.querySelector('#cfg-save').addEventListener('click', async () => {
    const status = box.querySelector('#cfg-status');
    const payload = { action: 'update_settings' };
    box.querySelectorAll('[data-name]').forEach((el) => {
      const kind = el.dataset.kind;
      payload[el.dataset.name] = kind === 'bool' ? el.checked
        : (kind === 'int' || kind === 'float') ? Number(el.value)
        : el.value;
    });
    status.textContent = 'Saving…';
    try {
      await post('/api/config', payload);
      // The action keeps its name through the flow: Save changes → Saved.
      status.textContent = 'Saved';
      status.style.color = 'var(--ok)';
    } catch (err) {
      status.textContent = `Didn't save: ${err.message}`;
      status.style.color = 'var(--bad)';
    }
  });
}
