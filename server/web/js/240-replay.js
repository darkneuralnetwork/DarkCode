// 240-replay.js — scrubbing through a recorded run.
//
// The execution journal exists so a crashed DAG can resume without paying for
// the model calls it already made. The same ordered record answers a different
// question after the fact: what did this run actually do, and where did it go
// wrong. That only needs a way to move through it.
//
// State at position i is derived by folding events 0..i rather than stored per
// frame, so scrubbing backwards is as cheap and as correct as scrubbing
// forwards — a timeline that only reconstructs going forward drifts the first
// time someone drags the handle left.

(function () {
  const KIND_STYLE = {
    run_started:    { icon: '▶', color: 'var(--text-dim)' },
    node_completed: { icon: '✓', color: 'var(--ok, #4ade80)' },
    node_failed:    { icon: '✗', color: 'var(--err, #f87171)' },
    run_finished:   { icon: '■', color: 'var(--text-dim)' },
  };

  // playIntervalMs paces playback. Fast enough to feel like playback, slow
  // enough to read a node name as it passes.
  const playIntervalMs = 600;

  let events = [];
  let position = 0;
  let timer = null;

  const $ = (id) => document.getElementById(id);

  function styleFor(kind) {
    return KIND_STYLE[kind] || { icon: '·', color: 'var(--text-mute)' };
  }

  async function loadRuns() {
    const select = $('rp-run');
    if (!select) return;
    try {
      const res = await fetch('/api/runs');
      const data = await res.json();
      const runs = data.runs || [];
      if (!runs.length) {
        select.innerHTML = '<option value="">No recorded runs yet</option>';
        return;
      }
      select.innerHTML = runs.map((r) => {
        const when = r.started ? new Date(r.started).toLocaleString() : '';
        const goal = (r.goal || r.id || '').slice(0, 70);
        return `<option value="${encodeURIComponent(r.goal || '')}">[${r.status}] ${goal} — ${when}</option>`;
      }).join('');
      loadRun(runs[0].goal || '');
    } catch (e) {
      select.innerHTML = '<option value="">Could not load runs</option>';
    }
  }

  async function loadRun(goal) {
    stop();
    if (!goal) return;
    try {
      const res = await fetch('/api/runs?goal=' + encodeURIComponent(goal));
      const data = await res.json();
      events = data.events || [];
    } catch (e) {
      events = [];
    }
    position = Math.max(0, events.length - 1);

    const show = events.length > 0 ? '' : 'none';
    ['rp-player', 'rp-detail-panel', 'rp-timeline-panel'].forEach((id) => {
      const el = $(id);
      if (el) el.style.display = show === '' ? 'block' : 'none';
    });

    const slider = $('rp-slider');
    if (slider) {
      slider.max = String(Math.max(0, events.length - 1));
      slider.value = String(position);
    }
    render();
  }

  // render redraws everything from `position`. Deriving the whole view each
  // time is what makes scrubbing in either direction consistent.
  function render() {
    if (!events.length) return;
    const ev = events[position];
    const st = styleFor(ev.kind);

    const pos = $('rp-position');
    if (pos) {
      pos.textContent = `${position + 1} / ${events.length} — ${ev.kind}` +
        (ev.name ? ` · ${ev.name}` : '');
      pos.style.color = st.color;
    }

    const elapsed = $('rp-elapsed');
    if (elapsed && events[0].time && ev.time) {
      const ms = new Date(ev.time) - new Date(events[0].time);
      elapsed.textContent = `+${(ms / 1000).toFixed(1)}s into the run`;
    }

    const detail = $('rp-detail');
    if (detail) {
      const parts = [];
      if (ev.time) parts.push(`time    ${new Date(ev.time).toLocaleTimeString()}`);
      parts.push(`kind    ${ev.kind}`);
      if (ev.node) parts.push(`node    ${ev.node}`);
      if (ev.name) parts.push(`name    ${ev.name}`);
      if (ev.error) parts.push(`\nerror\n${ev.error}`);
      if (ev.output) parts.push(`\noutput\n${ev.output}`);
      detail.textContent = parts.join('\n');
    }

    // Everything up to `position` has happened; the rest has not yet. Dimming
    // the future is the whole point of a scrubber.
    const timeline = $('rp-timeline');
    if (timeline) {
      timeline.innerHTML = events.map((e, i) => {
        const s = styleFor(e.kind);
        const past = i <= position;
        const label = e.node || e.name || e.kind;
        return `<div data-idx="${i}" style="display:flex;gap:8px;align-items:center;padding:3px 6px;
                  border-radius:4px;cursor:pointer;font-size:11px;font-family:var(--font-mono);
                  opacity:${past ? 1 : 0.32};
                  background:${i === position ? 'var(--bg-panel)' : 'transparent'};">
                  <span style="color:${s.color};width:12px;">${s.icon}</span>
                  <span style="color:var(--text-mute);width:56px;flex:none;overflow:hidden;
                        text-overflow:ellipsis;">${e.kind.replace('node_', '')}</span>
                  <span style="color:var(--text);flex:1;overflow:hidden;text-overflow:ellipsis;
                        white-space:nowrap;">${escapeHTML(String(label).slice(0, 80))}</span>
                </div>`;
      }).join('');
      const active = timeline.querySelector(`[data-idx="${position}"]`);
      if (active && active.scrollIntoView) active.scrollIntoView({ block: 'nearest' });
    }

    const status = $('rp-status');
    if (status) {
      const failed = events.slice(0, position + 1).filter((e) => e.kind === 'node_failed').length;
      status.textContent = failed ? `${failed} failure(s) so far` : '';
      status.style.color = failed ? 'var(--err, #f87171)' : 'var(--text-mute)';
    }
  }

  function escapeHTML(s) {
    return s.replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  function seek(i) {
    if (!events.length) return;
    position = Math.min(Math.max(0, i), events.length - 1);
    const slider = $('rp-slider');
    if (slider) slider.value = String(position);
    render();
  }

  function stop() {
    if (timer) clearInterval(timer);
    timer = null;
    const btn = $('rp-play');
    if (btn) btn.textContent = '▶';
  }

  function togglePlay() {
    if (timer) return stop();
    // Restarting from the end would otherwise sit there doing nothing.
    if (position >= events.length - 1) seek(0);
    const btn = $('rp-play');
    if (btn) btn.textContent = '⏸';
    timer = setInterval(() => {
      if (position >= events.length - 1) return stop();
      seek(position + 1);
    }, playIntervalMs);
  }

  function wire() {
    const on = (id, ev, fn) => {
      const el = $(id);
      if (el) el.addEventListener(ev, fn);
    };
    on('rp-run', 'change', (e) => loadRun(decodeURIComponent(e.target.value)));
    on('rp-refresh', 'click', loadRuns);
    on('rp-slider', 'input', (e) => { stop(); seek(Number(e.target.value)); });
    on('rp-first', 'click', () => { stop(); seek(0); });
    on('rp-prev', 'click', () => { stop(); seek(position - 1); });
    on('rp-next', 'click', () => { stop(); seek(position + 1); });
    on('rp-last', 'click', () => { stop(); seek(events.length - 1); });
    on('rp-play', 'click', togglePlay);

    const timeline = $('rp-timeline');
    if (timeline) {
      timeline.addEventListener('click', (e) => {
        const row = e.target.closest('[data-idx]');
        if (row) { stop(); seek(Number(row.dataset.idx)); }
      });
    }
  }

  // The page is injected when its tab is first shown, so wiring waits for the
  // element to exist rather than for DOMContentLoaded.
  function init() {
    if (!$('rp-run')) return false;
    wire();
    loadRuns();
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
