// main.js — boot and wiring.
//
// Every module is imported once here and connected to the live stream in one
// place, so "what happens when event X arrives" has a single answer you can
// read top to bottom. The previous UI answered that question across four files
// that each wrapped the others' handlers.

import { get, esc, fmtNum, fmtCost } from './api.js';
import * as stream from './stream.js';
import * as palette from './palette.js';
import * as chat from './chat.js';
import * as cascade from './cascade.js';
import * as views from './views.js';

const $ = (id) => document.getElementById(id);

/* ── Workspace aside ──────────────────────────────────────────────── */
const ASIDE_KEY = 'dc.aside';

function setAside(on) {
  $('work').dataset.aside = on ? 'on' : 'off';
  try { localStorage.setItem(ASIDE_KEY, on ? 'on' : 'off'); } catch { /* private mode */ }
}
function toggleAside() { setAside($('work').dataset.aside !== 'on'); }

async function loadTree() {
  const box = $('tree');
  try {
    const d = await get('/api/files/list');
    const files = d.files || d.entries || [];
    if (!files.length) { box.innerHTML = '<div class="empty">Nothing here yet.</div>'; return; }
    box.innerHTML = files.slice(0, 300).map((f) => {
      const name = typeof f === 'string' ? f : (f.path || f.name || '');
      const dir = typeof f === 'object' && (f.is_dir || f.type === 'dir');
      return `<button class="tree-row" data-path="${esc(name)}">
                <span style="color:var(--ink-mute)">${dir ? '›' : '·'}</span>
                <span>${esc(name.split('/').pop())}</span>
              </button>`;
    }).join('');
  } catch (err) {
    box.innerHTML = `<div class="empty">${esc(err.message)}</div>`;
  }
}

/* ── Rail ─────────────────────────────────────────────────────────── */
function railStage(text) {
  const el = $('rail-stage');
  el.textContent = text || '';
}

function railConn(state) {
  const el = $('rail-conn');
  el.textContent = state === 'up' ? 'live' : 'reconnecting';
  el.style.color = state === 'up' ? 'var(--ok)' : 'var(--warn)';
}

/* ── Boot ─────────────────────────────────────────────────────────── */
function boot() {
  chat.init();
  palette.init();
  cascade.mount(document.createElement('div'));

  // The cascade lives under the file tree in the aside: it belongs next to the
  // conversation, not on a dashboard, because it describes the turn happening
  // right now.
  const host = document.createElement('div');
  $('aside').appendChild(host);
  cascade.mount(host);

  try { setAside(localStorage.getItem(ASIDE_KEY) !== 'off'); } catch { setAside(true); }
  $('tree-refresh')?.addEventListener('click', loadTree);

  palette.register([
    { title: 'Chat', hint: 'esc', run: () => { views.showChat(); chat.focus(); } },
    { title: 'Memory — search what the agent knows', hint: 'search', run: views.memory },
    { title: 'Telemetry — cost, cascade, consensus', hint: 'cost', run: views.telemetry },
    { title: 'Settings', hint: 'config', run: views.settings },
    { title: 'Toggle the workspace', hint: '⌘B', run: toggleAside },
    { title: 'Clear the screen', run: () => { views.showChat(); chat.clear(); } },
    { title: 'New chat — clears memory of this conversation', run: async () => {
        views.showChat(); chat.clear();
        try { await fetch('/api/reset', { method: 'POST' }); } catch { /* offline */ }
        chat.turn('agent', 'Fresh session. What are we working on?');
      } },
    { title: 'Refresh the workspace', run: loadTree },
  ]);

  document.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'b') {
      const t = e.target;
      if (t && (t.tagName === 'TEXTAREA' || t.tagName === 'INPUT')) return;
      e.preventDefault(); toggleAside();
    }
  });

  // ── The live stream, wired once ────────────────────────────────────
  stream.on('connected', () => railConn('up'));
  stream.on('disconnected', () => railConn('down'));

  stream.on('task_update', (e) => {
    if (e.status === 'streaming' && e.content) { chat.appendDelta(String(e.content)); return; }
    cascade.fromEvent(e);
    if (e.content) railStage(String(e.content).slice(0, 60));
  });

  stream.on('model_route', (e) => {
    const m = String(e.content || '').match(/model=(\S+)/);
    if (m) $('rail-model').textContent = m[1];
  });

  stream.on('token_usage', (e) => {
    const s = (e && typeof e.content === 'object') ? e.content : {};
    $('rail-cost').textContent = `${fmtNum(s.cumulative_tokens || 0)} tok · ${fmtCost(s.cumulative_cost || 0)}`;
  });

  stream.on('tool_execution', (e) => {
    if (e.tool) railStage(`${e.tool} ${e.status || ''}`.trim());
  });

  stream.on('file_change', () => loadTree());
  stream.on('consensus', (e) => views.consensusRound(e));
  stream.on('final_output', () => railStage(''));
  stream.on('error', (e) => railStage(String(e.content || 'error').slice(0, 60)));

  document.addEventListener('dc:turn-start', () => { cascade.reset(); railStage('thinking'); });

  stream.connect();
  loadTree();
  views.showChat();
  chat.focus();
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();
