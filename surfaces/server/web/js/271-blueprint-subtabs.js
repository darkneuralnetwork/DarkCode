// 271-blueprint-subtabs.js — switches Blueprint between its three sub-panels.
//
// "Run Graph" and "Project Plan" are two different things that happen to share
// a tab (see blueprint.html's header comment); "Decisions" (formerly the
// Cognition Cascade tab) joined them in Phase 9 for the same reason — it's
// also "why the system did what it did", plan-adjacent. None of the three
// panels' own scripts need to know the others exist — 270-blueprint.js keeps
// polling in the background either way, 100-projects.js keeps listening on
// the event bus either way, and 260-cascade.js loads once at boot either
// way. This file's only job is which one is visible.

(function () {
  const TABS = [
    { tab: 'bp-subtab-run', panel: 'bp-panel-run' },
    { tab: 'bp-subtab-plan', panel: 'bp-panel-plan' },
    { tab: 'bp-subtab-decisions', panel: 'bp-panel-decisions' },
  ];

  function activate(tabId) {
    TABS.forEach(({ tab, panel }) => {
      const tabEl = document.getElementById(tab);
      const panelEl = document.getElementById(panel);
      if (!tabEl || !panelEl) return;
      const on = tab === tabId;
      tabEl.classList.toggle('active', on);
      tabEl.setAttribute('aria-selected', on ? 'true' : 'false');
      panelEl.hidden = !on;
    });
  }

  function init() {
    const first = document.getElementById(TABS[0].tab);
    if (!first) return false;
    TABS.forEach(({ tab }) => {
      const tabEl = document.getElementById(tab);
      if (tabEl) tabEl.addEventListener('click', () => activate(tab));
    });
    return true;
  }

  if (!init()) {
    const observer = new MutationObserver(() => { if (init()) observer.disconnect(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
