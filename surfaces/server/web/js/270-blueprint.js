// 270-blueprint.js — the live run: plan, task, consequence.
//
// The kernel has always built a plan graph before executing anything, and has
// always journalled what each node did. Neither was reachable from the browser,
// so the Blueprint tab could show a project's standing documents and nothing
// about the run actually happening.
//
// Three columns, because they answer three questions a reviewer asks in order:
// what is the shape of the work, what is this step doing, and what will it
// break. The last one is the point — approving a change you cannot see the
// consequence of is not really approving it.
//
// Status comes from the graph's own node.status rather than being inferred from
// the event stream. The events tell you *when* something happened; the graph is
// what the executor actually believes now, and a view that disagrees with the
// executor is worse than one that lags it.

(function () {
  const $ = (id) => document.getElementById(id);

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  // Status vocabulary is the executor's, not ours: core.TaskStatus values.
  const STATUS = {
    completed: { cls: 'ok',      dot: '●', label: 'done' },
    running:   { cls: 'running', dot: '◐', label: 'running' },
    failed:    { cls: 'fail',    dot: '✕', label: 'failed' },
    blocked:   { cls: 'blocked', dot: '⊘', label: 'blocked' },
    pending:   { cls: 'idle',    dot: '○', label: 'pending' },
  };
  const statusOf = (n) => STATUS[String(n.status || 'pending').toLowerCase()] || STATUS.pending;

  let graph = null;      // the plan as the executor sees it
  let events = [];       // this run's journal
  let selected = null;   // node id the user is inspecting

  // ── the plan column ──────────────────────────────────────────────────
  //
  // Dependencies are drawn as text rather than as lines. A real DAG rendering
  // needs a layout engine, and at the handful of nodes a plan actually has, the
  // dependency list is easier to read than crossing edges would be.
  function renderNodes() {
    const box = $('bp-nodes');
    if (!box) return;
    const nodes = (graph && graph.nodes) || [];
    $('bp-node-count').textContent = nodes.length ? `${nodes.filter(n => statusOf(n).cls === 'ok').length}/${nodes.length}` : '';

    if (!nodes.length) {
      box.innerHTML = '<div class="bp-empty">Nothing running.</div>';
      return;
    }
    box.innerHTML = nodes.map((n) => {
      const st = statusOf(n);
      const deps = (n.deps || []).length ? `<div class="bp-node-deps">after ${(n.deps || []).map(esc).join(', ')}</div>` : '';
      return `<button class="bp-node ${st.cls}${selected === n.id ? ' selected' : ''}" data-node="${esc(n.id)}">
          <span class="bp-node-dot">${st.dot}</span>
          <span class="bp-node-main">
            <span class="bp-node-id">${esc(n.id)}</span>
            <span class="bp-node-name">${esc(n.name || n.goal || '')}</span>
            ${deps}
          </span>
          <span class="bp-node-agent">${esc(n.agent || '')}</span>
        </button>`;
    }).join('');

    box.querySelectorAll('[data-node]').forEach((el) => {
      el.addEventListener('click', () => { selected = el.dataset.node; renderNodes(); renderTask(); renderImpact(); });
    });
  }

  // ── the task column ──────────────────────────────────────────────────
  function renderTask() {
    const body = $('bp-task-body');
    if (!body) return;
    const node = ((graph && graph.nodes) || []).find((n) => n.id === selected);
    if (!node) {
      $('bp-task-title').textContent = 'Task';
      $('bp-task-agent').textContent = '';
      body.innerHTML = '<div class="bp-empty">Select a task to see its goal, acceptance criteria and what it produced.</div>';
      return;
    }
    $('bp-task-title').textContent = node.id;
    $('bp-task-agent').textContent = node.agent || '';

    const parts = [`<div class="bp-goal-text">${esc(node.goal || node.name)}</div>`];

    // Acceptance is the whole verifier-first idea made visible: these are the
    // conditions the executor will check, not a description of intent.
    if ((node.acceptance || []).length) {
      parts.push(`<div class="bp-sec"><div class="bp-sec-h">Accepted when</div><ul class="bp-list">${
        node.acceptance.map((a) => `<li>${esc(a)}</li>`).join('')}</ul></div>`);
    }
    if ((node.artifacts || []).length) {
      parts.push(`<div class="bp-sec"><div class="bp-sec-h">Expected to produce</div><ul class="bp-list mono">${
        node.artifacts.map((a) => `<li>${esc(a)}</li>`).join('')}</ul></div>`);
    }

    // Proof is the difference between a task that says it is done and one that
    // has shown it. Acceptance above is intent — the conditions the executor
    // will check; this is what happened when it checked them, with the command
    // and its real output. It was already in the payload and never rendered,
    // which left the panel showing only the claim.
    if ((node.proof || []).length) {
      parts.push(`<div class="bp-sec"><div class="bp-sec-h">Proof</div>${
        node.proof.map((p) => {
          // A criterion with no command was recorded as unverified rather than
          // quietly passed. Showing it as neither is the honest rendering:
          // absence of evidence must not look like evidence.
          if (!p.command) {
            return `<div class="bp-evt"><span class="bp-evt-kind">unverified</span>
                <span class="bp-evt-time">${esc(p.criterion || '')}</span></div>`;
          }
          return `<div class="bp-evt ${p.passed ? '' : 'fail'}">
              <span class="bp-evt-kind">${p.passed ? '✓ passed' : '✕ failed'}</span>
              <span class="bp-evt-time mono">${esc(p.command)}</span>
              ${p.output && !p.passed ? `<pre class="bp-evt-detail">${esc(String(p.output).slice(0, 1200))}</pre>` : ''}
            </div>`;
        }).join('')}</div>`);
    }

    // What the journal recorded for this node.
    const mine = events.filter((e) => e.node === node.id || e.name === node.name);
    if (mine.length) {
      parts.push(`<div class="bp-sec"><div class="bp-sec-h">What happened</div>${
        mine.map((e) => {
          const when = e.time ? new Date(e.time).toLocaleTimeString() : '';
          const detail = e.error || e.output || '';
          return `<div class="bp-evt ${e.error ? 'fail' : ''}">
              <span class="bp-evt-kind">${esc(e.kind)}</span>
              <span class="bp-evt-time">${esc(when)}</span>
              ${detail ? `<pre class="bp-evt-detail">${esc(detail.slice(0, 1200))}</pre>` : ''}
            </div>`;
        }).join('')}</div>`);
    } else {
      parts.push('<div class="bp-sec"><div class="bp-sec-h">What happened</div><div class="bp-empty">Not started.</div></div>');
    }
    body.innerHTML = parts.join('');
  }

  // ── the consequence column ───────────────────────────────────────────
  //
  // Two things belong here and they arrive from different places: what the run
  // has already touched (checkpoints), and what it is asking permission to do
  // (the approval queue). Both are answers to "should I let this proceed".
  async function renderImpact() {
    const body = $('bp-impact-body');
    if (!body) return;
    const parts = [];

    try {
      const res = await fetch('/api/approvals');
      const data = await res.json();
      const pending = data.approvals || [];
      $('bp-impact-count').textContent = pending.length ? `${pending.length} waiting` : '';
      if (pending.length) {
        parts.push(`<div class="bp-sec"><div class="bp-sec-h bp-warn">Waiting on you</div>${
          pending.map((a) => `<div class="bp-approval">
              <div class="bp-approval-tool">${esc(a.tool || 'tool')}</div>
              <div class="bp-approval-sum">${esc(a.summary || '')}</div>
            </div>`).join('')}</div>`);
      }
    } catch (e) { /* the panel is still useful without it */ }

    try {
      const res = await fetch('/api/checkpoints');
      const data = await res.json();
      const cps = (data.checkpoints || data.entries || []).slice(-6).reverse();
      if (cps.length) {
        parts.push(`<div class="bp-sec"><div class="bp-sec-h">Files this run touched</div>${
          cps.map((c) => `<div class="bp-cp">
              <span class="bp-cp-id">#${esc(c.id)}</span>
              <span class="bp-cp-label">${esc(c.label || c.tool || '')}</span>
            </div>`).join('')}</div>`);
      }
    } catch (e) { /* ditto */ }

    body.innerHTML = parts.length
      ? parts.join('')
      : '<div class="bp-empty">Nothing pending, and nothing changed yet.</div>';
  }

  function renderHeader() {
    const goalEl = $('bp-goal');
    const state = $('bp-state');
    if (!graph) {
      if (goalEl) goalEl.textContent = 'No run yet — ask for something and its plan appears here.';
      if (state) state.hidden = true;
      const fill = $('bp-progress-fill');
      if (fill) fill.style.width = '0%';
      return;
    }
    if (goalEl) goalEl.textContent = graph.goal || '';
    const nodes = graph.nodes || [];
    const done = nodes.filter((n) => statusOf(n).cls === 'ok').length;
    const fill = $('bp-progress-fill');
    if (fill) fill.style.width = nodes.length ? `${Math.round((done / nodes.length) * 100)}%` : '0%';

    if (state) {
      // A plan awaiting approval is the one state a viewer must not miss.
      state.hidden = false;
      state.textContent = graph.__pending ? 'awaiting approval' : (graph.depth || 'executed');
      state.className = 'bp-badge' + (graph.__pending ? ' pending' : '');
    }
  }

  async function load() {
    try {
      const res = await fetch('/api/plan');
      const data = await res.json();
      graph = data.plan || null;
      if (graph) graph.__pending = !!data.pending;
    } catch (e) { graph = null; }

    events = [];
    if (graph && graph.goal) {
      try {
        const res = await fetch('/api/runs?goal=' + encodeURIComponent(graph.goal));
        const data = await res.json();
        events = data.events || [];
      } catch (e) { /* a plan without a journal still renders */ }
    }

    // Default to the step a reader most likely wants: the one running, else the
    // first that has not finished, else the first.
    const nodes = (graph && graph.nodes) || [];
    if (nodes.length && !nodes.some((n) => n.id === selected)) {
      const running = nodes.find((n) => statusOf(n).cls === 'running');
      const next = nodes.find((n) => statusOf(n).cls !== 'ok');
      selected = (running || next || nodes[0]).id;
    }

    renderHeader();
    renderNodes();
    renderTask();
    renderImpact();
  }

  function init() {
    if (!$('bp-nodes')) return false;
    const btn = $('blueprint-refresh');
    if (btn) btn.addEventListener('click', load);
    load();
    // The plan changes while a run is in flight; a stale board is the failure
    // mode this panel exists to avoid.
    // Guarded on BOTH the tab being active and the document being visible.
    // This checked only the tab, so a backgrounded browser left on Blueprint
    // kept firing four requests every four seconds — /api/plan, /api/runs,
    // /api/approvals and /api/checkpoints — indefinitely. Every other poller
    // in this UI already checks document.hidden; this one was the exception.
    setInterval(() => {
      const panel = document.getElementById('tab-blueprint');
      if (panel && panel.classList.contains('active') && !document.hidden) load();
    }, 4000);
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
