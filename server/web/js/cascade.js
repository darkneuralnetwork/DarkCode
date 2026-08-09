// cascade.js — the signature element.
//
// A question descends six rungs, and every rung that answers is a model call
// that never happened. That mechanic is the product's actual argument — it is
// why this tool costs less to run than one that sends everything to a frontier
// model — and in the previous interface it was a bar chart on a dashboard tab
// nobody opened.
//
// Here it lives beside the conversation and animates in real time: you watch
// the question fall, and watch it stop. When it stops early, the rungs below
// dim out and the saving is stated in place.
//
// The rungs are numbered because they ARE an ordered descent. The number is
// the depth a question reached, not decoration — which is the only thing that
// justifies ordinal markers in an interface.

import { esc } from './api.js';

const RUNGS = [
  { id: 'smalltalk',     label: 'smalltalk',     note: 'greeting, no retrieval' },
  { id: 'deterministic', label: 'deterministic', note: 'answerable from a tool' },
  { id: 'cache',         label: 'cache',         note: 'asked before' },
  { id: 'graph',         label: 'graph',         note: 'the code graph knows' },
  { id: 'recall',        label: 'recall',        note: 'memory knows' },
  { id: 'llm',           label: 'model',         note: 'ask the model' },
];

let host = null;

export function mount(el) {
  host = el;
  host.innerHTML = `
    <div class="eyebrow"><span>cascade</span><span id="cascade-verdict"></span></div>
    <div class="cascade">
      ${RUNGS.map((r, i) => `
        <div class="rung" data-rung="${r.id}" data-state="idle" title="${esc(r.note)}">
          <span class="rung-dot"></span>
          <span><span class="rung-n">${i}</span> ${esc(r.label)}</span>
          <span class="rung-saved">answered</span>
        </div>`).join('')}
    </div>`;
  reset();
}

export function reset() {
  if (!host) return;
  host.querySelectorAll('.rung').forEach((el) => (el.dataset.state = 'idle'));
  const v = host.querySelector('#cascade-verdict');
  if (v) v.textContent = '';
}

/** enter marks a rung as being tried; everything above it has been passed. */
export function enter(id) {
  if (!host) return;
  const idx = RUNGS.findIndex((r) => r.id === id);
  if (idx < 0) return;
  host.querySelectorAll('.rung').forEach((el, i) => {
    el.dataset.state = i < idx ? 'passed' : i === idx ? 'active' : 'idle';
  });
}

/** answered stops the descent at a rung and says what it saved. */
export function answered(id) {
  if (!host) return;
  const idx = RUNGS.findIndex((r) => r.id === id);
  if (idx < 0) return;
  host.querySelectorAll('.rung').forEach((el, i) => {
    el.dataset.state = i < idx ? 'passed' : i === idx ? 'answered' : 'idle';
  });
  const v = host.querySelector('#cascade-verdict');
  if (!v) return;
  // The claim, stated only when it is true: reaching the model is the
  // expensive outcome, so it gets no celebration.
  v.textContent = id === 'llm' ? '' : `saved ${RUNGS.length - 1 - idx} step(s)`;
  v.style.color = 'var(--ok)';
}

/**
 * fromEvent reads the kernel's own cascade telemetry off a task_update.
 *
 * The kernel logs lines like "Rung 0 (smalltalk) answered without LLM: …".
 * Parsing its own words rather than inventing a second event shape keeps the
 * display honest — if the wording changes the display goes quiet instead of
 * lying about a rung that did not run.
 */
export function fromEvent(evt) {
  const text = typeof evt?.content === 'string' ? evt.content : '';
  const m = text.match(/Rung\s+(\d+)\s*\(([a-z]+)\)/i);
  if (!m) return;
  const id = m[2].toLowerCase();
  if (/answered/i.test(text)) answered(id);
  else enter(id);
}
