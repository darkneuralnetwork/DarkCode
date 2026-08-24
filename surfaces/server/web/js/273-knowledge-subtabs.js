// 273-knowledge-subtabs.js — switches Knowledge between its two sub-panels
// (Memory, Tools). Same pattern as 271-blueprint-subtabs.js / 272-telemetry-
// subtabs.js; kept separate rather than generalized — see 272's header
// comment for why.

(function () {
  const TABS = [
    { tab: 'kn-subtab-memory', panel: 'kn-panel-memory' },
    { tab: 'kn-subtab-tools', panel: 'kn-panel-tools' },
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
