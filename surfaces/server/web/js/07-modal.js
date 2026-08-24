// 07-modal.js — shared modal shell behavior: Escape-to-close, and a real
// close on backdrop click.
//
// Every overlay in this app (project, context editor, directory picker, file
// viewer, maximize, CLI-switch) opens through its own hand-rolled logic —
// each with real cleanup behind it (closeProjectModal resets editing state,
// closeDirPicker clears picker state, and so on). None of them closed on
// Escape; the pointer was the only way out of any of them. This file doesn't
// touch how a modal opens — it adds the one shell behavior every dialog here
// should share, and it calls back into each modal's own close function so
// that cleanup still runs, rather than reimplementing "hide the overlay"
// a second way.
//
// A generic backdrop-click handler already existed (150-events.js: any click
// whose target carries the .perm-overlay class hides that element) — but it
// hides by setting style.display directly, which is a second, DIFFERENT way
// of closing that skips each modal's real close function. For proj-modal,
// clicking outside it never reset projEditingId; the next "+ New" open could
// silently reopen in edit mode. Registering here doesn't remove that old
// handler (harmless once this one has already run — it just re-applies
// style.display=none to an element that's already closed), it just makes
// sure the real close function runs first, on the same registry Escape uses.
//
// #perm-overlay (the tool-approval modal) is deliberately never registered
// here. It gates a tool call the orchestrator is blocked on; Escape or a
// stray backdrop click silently dismissing it would leave that wait
// unresolved with no decision recorded. Allow Once / Allow Session / Deny
// stay its only way to close — its pre-existing backdrop-click-to-dismiss
// behavior (150-events.js) is untouched here, not extended.

(function () {
  const registry = [];

  function register(overlayId, closeFn) {
    registry.push({ overlayId, closeFn });
  }

  function isOpen(overlayId) {
    const el = document.getElementById(overlayId);
    if (!el) return false;
    if (el.classList.contains("active")) return true;
    const display = el.style.display;
    return display !== "" && display !== "none";
  }

  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    // Reverse registration order: a modal opened on top of another (e.g.
    // maximize opened while a project modal sits behind it) closes first.
    for (let i = registry.length - 1; i >= 0; i--) {
      const { overlayId, closeFn } = registry[i];
      if (isOpen(overlayId)) {
        closeFn();
        return;
      }
    }
  });

  // Backdrop click: only when the click landed on the overlay itself, not a
  // descendant (the dialog body) — the same test the pre-existing generic
  // handler in 150-events.js uses. Registered here (loads before
  // 150-events.js) so the real close function runs first.
  document.addEventListener("click", (e) => {
    for (const { overlayId, closeFn } of registry) {
      if (e.target.id === overlayId && isOpen(overlayId)) {
        closeFn();
        return;
      }
    }
  });

  window.ModalShell = { register, isOpen };

  // Trivial cases with no dedicated close function elsewhere — registered
  // directly here rather than inventing a named function in index.html's
  // inline handler code for a single line of cleanup.
  register("maximize-modal", () => {
    const el = document.getElementById("maximize-modal");
    if (el) el.style.display = "none";
  });
  register("cli-modal", () => {
    const el = document.getElementById("cli-modal");
    if (el) el.style.display = "none";
  });
})();
