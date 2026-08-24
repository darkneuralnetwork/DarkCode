// 272-telemetry-subtabs.js — switches Telemetry between its four sub-panels
// (Live, Metrics, History, Audit). Same pattern as 271-blueprint-subtabs.js;
// kept as its own small file rather than generalizing the two into one
// driver — a typo in a shared config table would silently break every tab
// using it, where a typo here breaks only this one.

(function () {
  const TABS = [
    { tab: 'tel-subtab-live', panel: 'tel-panel-live' },
    { tab: 'tel-subtab-metrics', panel: 'tel-panel-metrics' },
    { tab: 'tel-subtab-history', panel: 'tel-panel-history' },
    { tab: 'tel-subtab-audit', panel: 'tel-panel-audit' },
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
