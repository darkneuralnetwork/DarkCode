// 290-plugins.js — the plugin manifests the loader found.
//
// A plugin is a JSON manifest in the workspace's plugins/ directory declaring
// the capabilities it registers. The loader has always read them and
// /api/plugins has always listed them; nothing displayed the result, so a
// manifest that was never picked up — wrong directory, bad JSON, missing field
// — looked exactly like one that loaded perfectly. The only way to tell was to
// notice a tool you expected was absent.
//
// Showing what was discovered makes the negative case visible, which is the
// case worth showing: an empty list where you expected an entry is a fact, and
// silence is not.

(function () {
  const $ = (id) => document.getElementById(id);

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
      { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    ));
  }

  function render(plugins) {
    const box = $('plugins-list');
    if (!box) return;
    const stats = $('plugins-stats');

    if (!plugins.length) {
      box.innerHTML = '<div class="mem-empty" style="padding:12px;">' +
        'No plugins found. Drop a manifest into <code>plugins/</code> in the workspace and rescan.</div>';
      if (stats) stats.textContent = 'none';
      return;
    }

    const caps = plugins.reduce((n, p) => n + ((p.registers || []).length), 0);
    if (stats) stats.textContent = `${plugins.length} plugin${plugins.length > 1 ? 's' : ''} · ${caps} registered`;

    box.innerHTML = plugins.map((p) => {
      const regs = p.registers || [];
      return `<div class="model-row">
        <div class="model-row-main">
          <span class="model-name">${esc(p.name || 'unnamed')}</span>
          <span class="model-meta">v${esc(p.version || '—')}</span>
        </div>
        <div class="plugin-caps">${
          regs.length
            ? regs.map((r) => `<span class="plugin-cap" title="${esc(r.type || 'capability')}">${esc(r.id || '?')}</span>`).join('')
            : '<span class="plugin-cap plugin-cap-none">registers nothing</span>'
        }</div>
      </div>`;
    }).join('');
  }

  async function load() {
    try {
      const res = await fetch('/api/plugins');
      const data = await res.json();
      render(data.plugins || []);
    } catch (e) {
      const box = $('plugins-list');
      if (box) box.innerHTML = '<div class="mem-empty" style="padding:12px;">Could not read the plugin list.</div>';
    }
  }

  function init() {
    if (!$('plugins-list')) return false;
    const btn = $('plugins-refresh');
    // Rescanning is a directory read, so it is a button rather than a poll.
    if (btn) btn.addEventListener('click', load);
    load();
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
