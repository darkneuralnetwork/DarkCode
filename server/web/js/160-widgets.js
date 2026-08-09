/* 160-widgets.js — UNIFIED CUSTOM DROPDOWNS & AUTOCOMPLETE
 *
 * Replaces the three competing implementations that previously fought over
 * the same <select>/<input[list]> elements:
 *
 *   • initCustomSelects(container)         — targeted select.custom-select
 *   • IIFE convertSelectsToCustomDropdowns  — targeted select.glass-input
 *   • IIFE convertDatalistsToCustom         — targeted input[list]
 *
 * Problems with the old code:
 *   1. Two different class conventions (custom-select vs glass-input) for the
 *      same feature, so the config.html selects and the per-model role select
 *      were handled by different code paths with different wrapper class names.
 *   2. A GLOBAL monkeypatch of HTMLSelectElement.prototype.value that fired a
 *      'programmaticChange' event on every value assignment app-wide — fragile,
 *      can loop, and affects unrelated selects (incl. Chart.js internals).
 *   3. Two setInterval(...,500) loops scanning the entire DOM twice a second
 *      forever, even when nothing changed — a constant CPU/reflow cost.
 *
 * This single implementation:
 *   • handles BOTH select.custom-select AND select.glass-input (the two
 *     conventions used across the codebase) with one code path,
 *   • is idempotent (data-dc-select="1" guard) so re-init on dynamic forms
 *     never double-wraps,
 *   • refreshes its option list on open (so dynamically added options appear),
 *   • is driven by a single scoped MutationObserver instead of polling,
 *   • exposes window.DC.refreshSelect(select) / refreshDatalist(input) for
 *     code that rebuilds a select's options (model fetch, config load).
 *
 * The wrapper keeps the OLD class names (.custom-select-container / .trigger /
 * .options / .option) so styles.css continues to style it unchanged.
 */
"use strict";

(function () {
  if (window.DC && window.DC.widgetsLoaded) return;

  const DC = window.DC || (window.DC = {});
  DC.widgetsLoaded = true;

  // ── refreshSelect: rebuild a custom dropdown's option list from its
  //    underlying <select>, keeping the trigger label in sync. Call this
  //    after programmatically replacing a select's <option> children.
  DC.refreshSelect = function (select) {
    if (!select || select.dataset.dcSelect !== "1") return;
    const wrapper = select.closest(".custom-select-container");
    if (!wrapper) return;
    const trigger = wrapper.querySelector(".custom-select-trigger");
    const optionsContainer = wrapper.querySelector(".custom-select-options");
    if (!optionsContainer) return;
    rebuildOptions(select, optionsContainer, wrapper, trigger);
    const selOpt = select.options[select.selectedIndex];
    if (trigger && selOpt) trigger.textContent = selOpt.text;
  };

  function rebuildOptions(select, optionsContainer, wrapper, trigger) {
    optionsContainer.innerHTML = "";
    Array.from(select.options).forEach((opt) => {
      const optDiv = document.createElement("div");
      optDiv.className = "custom-option";
      if (opt.selected) optDiv.classList.add("selected");
      optDiv.textContent = opt.text;
      optDiv.dataset.value = opt.value;
      optDiv.addEventListener("click", (e) => {
        e.stopPropagation();
        select.value = opt.value;
        if (trigger) trigger.textContent = opt.text;
        wrapper.classList.remove("open");
        wrapper.querySelectorAll(".custom-option").forEach((o) => o.classList.remove("selected"));
        optDiv.classList.add("selected");
        select.dispatchEvent(new Event("change", { bubbles: true }));
      });
      optionsContainer.appendChild(optDiv);
    });
  }

  function enhanceSelect(select) {
    if (!select || select.dataset.dcSelect === "1") return;
    // Only enhance the two conventions actually used in the codebase.
    if (!select.classList.contains("custom-select") && !select.classList.contains("glass-input")) return;

    select.dataset.dcSelect = "1";
    select.style.display = "none";

    const wrapper = document.createElement("div");
    wrapper.className = "custom-select-container glass-input";

    const trigger = document.createElement("div");
    trigger.className = "custom-select-trigger";
    const selectedOpt = select.options[select.selectedIndex];
    trigger.textContent = selectedOpt ? selectedOpt.text : "Select...";

    const optionsContainer = document.createElement("div");
    optionsContainer.className = "custom-select-options custom-scrollbar";
    rebuildOptions(select, optionsContainer, wrapper, trigger);

    trigger.addEventListener("click", (e) => {
      e.stopPropagation();
      document.querySelectorAll(".custom-select-container.open").forEach((c) => {
        if (c !== wrapper) c.classList.remove("open");
      });
      // Re-read options on open in case they changed since last render.
      rebuildOptions(select, optionsContainer, wrapper, trigger);
      wrapper.classList.toggle("open");
    });

    // Keep the trigger label in sync when the underlying select changes
    // (programmatic or user-driven). Replaces the global prototype patch.
    select.addEventListener("change", () => {
      const selOpt = select.options[select.selectedIndex];
      if (trigger && selOpt) trigger.textContent = selOpt.text;
      wrapper.querySelectorAll(".custom-option").forEach((o) => {
        o.classList.toggle("selected", o.dataset.value === select.value);
      });
    });

    wrapper.appendChild(trigger);
    wrapper.appendChild(optionsContainer);
    select.parentNode.insertBefore(wrapper, select);
    wrapper.appendChild(select);
  }

  // ── Datalist autocomplete (input[list]). Keeps the .custom-select-wrapper /
  //    .custom-select-options / .custom-select-option classes used by the old
  //    IIFE so existing styling (if any) still applies.
  function enhanceDatalist(input) {
    if (!input || input.dataset.dcDatalist === "1") return;
    if (!input.hasAttribute("list")) return;
    const listId = input.getAttribute("list");
    const datalist = document.getElementById(listId);
    if (!datalist) return;

    input.dataset.dcDatalist = "1";
    // Remove the native list attribute to suppress the native UI (matches old behaviour).
    input.removeAttribute("list");

    const wrapper = document.createElement("div");
    wrapper.className = "custom-select-wrapper custom-datalist-wrapper";
    input.parentNode.insertBefore(wrapper, input);
    wrapper.appendChild(input);

    const optionsDiv = document.createElement("div");
    optionsDiv.className = "custom-select-options glass-panel custom-scrollbar";
    wrapper.appendChild(optionsDiv);

    function showOptions(forceAll = false) {
      optionsDiv.innerHTML = "";
      const val = forceAll ? "" : input.value.toLowerCase();
      let hasMatches = false;
      Array.from(datalist.options).forEach((option) => {
        const text = option.text || option.value;
        const valText = option.value;
        if (val === "" || text.toLowerCase().includes(val) || valText.toLowerCase().includes(val)) {
          hasMatches = true;
          const opt = document.createElement("div");
          opt.className = "custom-select-option";
          opt.textContent = text;
          opt.addEventListener("mousedown", (e) => {
            e.preventDefault(); // fire before input blur
            input.value = valText;
            input.dispatchEvent(new Event("change"));
            optionsDiv.classList.remove("open");
          });
          optionsDiv.appendChild(opt);
        }
      });
      optionsDiv.classList.toggle("open", hasMatches);
    }

    input.addEventListener("click", (e) => { e.stopPropagation(); showOptions(true); });
    optionsDiv.addEventListener("click", (e) => e.stopPropagation());
    optionsDiv.addEventListener("mousedown", (e) => e.stopPropagation());
    input.addEventListener("focus", () => { if (!input.dataset.ignoreFocus) showOptions(true); });
    input.addEventListener("input", () => showOptions());
    input.addEventListener("blur", () => { optionsDiv.classList.remove("open"); });

    const observer = new MutationObserver(() => {
      if (optionsDiv.classList.contains("open")) showOptions();
    });
    observer.observe(datalist, { childList: true });
  }

  // ── Public scan: walk a container once and enhance everything found.
  //    Idempotent (guards prevent re-wrap). Used by init() after fragments load.
  DC.scanWidgets = function (root) {
    root = root || document;
    root.querySelectorAll("select.custom-select, select.glass-input").forEach(enhanceSelect);
    root.querySelectorAll("input[list]").forEach(enhanceDatalist);
  };

  // ── Global click-away: close any open custom dropdown.
  document.addEventListener("click", () => {
    document.querySelectorAll(".custom-select-container.open").forEach((c) => c.classList.remove("open"));
    document.querySelectorAll(".custom-select-options.open").forEach((d) => d.classList.remove("open"));
    document.querySelectorAll(".custom-select-trigger.open").forEach((d) => d.classList.remove("open"));
  });

  // ── MutationObserver: enhance selects/datalists added dynamically (project
  //    modal, provider modal, config cards) WITHOUT a polling interval.
  //    Scoped to body, childList + subtree, so it only fires on actual DOM
  //    mutations — a constant-time observer, not a 500ms full-DOM scan.
  const obs = new MutationObserver((mutations) => {
    for (const m of mutations) {
      for (const node of m.addedNodes) {
        if (node.nodeType !== 1) continue;
        if (node.matches && node.matches("select.custom-select, select.glass-input")) enhanceSelect(node);
        if (node.matches && node.matches("input[list]")) enhanceDatalist(node);
        if (node.querySelectorAll) {
          node.querySelectorAll("select.custom-select, select.glass-input").forEach(enhanceSelect);
          node.querySelectorAll("input[list]").forEach(enhanceDatalist);
        }
      }
    }
  });
  // Start once the DOM is ready.
  if (document.body) {
    obs.observe(document.body, { childList: true, subtree: true });
    DC.scanWidgets(document);
  } else {
    document.addEventListener("DOMContentLoaded", () => {
      obs.observe(document.body, { childList: true, subtree: true });
      DC.scanWidgets(document);
    });
  }

  // Backward-compat shims: old code called these; keep them as no-ops so any
  // stray reference doesn't throw. The unified MutationObserver now does the
  // work they used to do via setInterval polling.
  window.convertSelectsToCustomDropdowns = function () { DC.scanWidgets(document); };
  window.convertDatalistsToCustom = function () { DC.scanWidgets(document); };
})();
