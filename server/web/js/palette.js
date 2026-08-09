// palette.js — the only navigation.
//
// The previous console had a nine-item dropdown in three groups, later six.
// Every one of those was a place to go, and a developer already knows how to
// go places: type where you want to be. Removing the nav bar is what lets the
// conversation own the whole window.
//
// Actions live in the same list as destinations. There is no reason "Clear the
// screen" should be a button in one corner while "Memory" is a menu item in
// another — both are things you want, found the same way.

import { esc } from './api.js';

let scrim, input, list, items = [], filtered = [], cursor = 0;

export function register(commands) {
  items = commands;
}

export function init() {
  scrim = document.getElementById('palette-scrim');
  input = document.getElementById('palette-input');
  list = document.getElementById('palette-list');

  document.getElementById('palette-open')?.addEventListener('click', open);
  scrim.addEventListener('mousedown', (e) => { if (e.target === scrim) close(); });
  input.addEventListener('input', () => { cursor = 0; render(); });
  input.addEventListener('keydown', onKey);

  // ⌘K / Ctrl+K anywhere, and Escape to leave. Not captured while typing a
  // message unless the modifier is held, so the composer keeps every key.
  document.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      scrim.hidden ? open() : close();
    } else if (e.key === 'Escape' && !scrim.hidden) {
      close();
    }
  });
}

export function open() {
  scrim.hidden = false;
  input.value = '';
  cursor = 0;
  render();
  input.focus();
}

export function close() {
  scrim.hidden = true;
  input.blur();
}

function onKey(e) {
  if (e.key === 'ArrowDown' || (e.key === 'n' && e.ctrlKey)) {
    e.preventDefault(); cursor = Math.min(cursor + 1, filtered.length - 1); render();
  } else if (e.key === 'ArrowUp' || (e.key === 'p' && e.ctrlKey)) {
    e.preventDefault(); cursor = Math.max(cursor - 1, 0); render();
  } else if (e.key === 'Enter') {
    e.preventDefault(); run(filtered[cursor]);
  }
}

function run(cmd) {
  if (!cmd) return;
  close();
  try { cmd.run(); } catch (err) { console.error('command failed:', err); }
}

/**
 * match is a subsequence test, not a substring one: "mem" finds "Memory" and
 * "tm" finds "Telemetry". Developers type initials, and a palette that only
 * does substrings makes them type more than the menu they replaced.
 */
function match(needle, hay) {
  needle = needle.toLowerCase(); hay = hay.toLowerCase();
  if (!needle) return true;
  let i = 0;
  for (const ch of hay) if (ch === needle[i] && ++i === needle.length) return true;
  return false;
}

function render() {
  const q = input.value.trim();
  filtered = items.filter((c) => match(q, c.title + ' ' + (c.hint || '')));
  cursor = Math.max(0, Math.min(cursor, filtered.length - 1));

  if (!filtered.length) {
    list.innerHTML = `<div class="palette-empty">Nothing matches “${esc(q)}”.</div>`;
    return;
  }
  list.innerHTML = filtered.map((c, i) => `
    <button class="palette-item" role="option" data-i="${i}" aria-selected="${i === cursor}">
      <span>${esc(c.title)}</span>
      ${c.hint ? `<span class="palette-item-key">${esc(c.hint)}</span>` : ''}
    </button>`).join('');

  list.querySelectorAll('.palette-item').forEach((el) => {
    el.addEventListener('click', () => run(filtered[Number(el.dataset.i)]));
    el.addEventListener('mousemove', () => {
      const i = Number(el.dataset.i);
      if (i !== cursor) { cursor = i; render(); }
    });
  });
  list.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' });
}
