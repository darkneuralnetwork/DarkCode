/* 75-verbs.js — the "/" command picker in the composer.
 *
 * The composer has shown a "/ for Commands" hint since it was written, with
 * nothing behind it. The verbs it should have been offering existed only in the
 * console, so `/loop fix the parser` typed here was sent to the model as
 * literal text.
 *
 * The list is fetched from /api/verbs rather than written here, because a
 * second copy of it is exactly how the two surfaces drifted apart the first
 * time.
 */

let verbList = null;
let verbActive = -1;

async function loadVerbs() {
  if (verbList) return verbList;
  try {
    const res = await fetch(API + "/api/verbs");
    const data = await res.json();
    verbList = Array.isArray(data.verbs) ? data.verbs : [];
  } catch {
    verbList = []; // no picker is better than a wrong picker
  }
  return verbList;
}

function verbMenuEl() {
  let el = $("#verb-menu");
  if (el) return el;
  const row = document.querySelector("#chat-form .chat-input-row");
  if (!row) return null;
  el = document.createElement("div");
  el.id = "verb-menu";
  el.className = "verb-menu glass-panel";
  el.hidden = true;
  row.appendChild(el);
  return el;
}

function hideVerbMenu() {
  const el = $("#verb-menu");
  if (el) el.hidden = true;
  verbActive = -1;
}

function verbMenuOpen() {
  const el = $("#verb-menu");
  return el && !el.hidden;
}

// verbQuery returns the partial verb being typed, or null. Only a "/" at the
// very start counts: mid-sentence slashes are paths, not commands.
function verbQuery(text) {
  if (!text.startsWith("/")) return null;
  const word = text.slice(1);
  if (/\s/.test(word)) return null; // the verb is complete; they're typing the task
  return word.toLowerCase();
}

async function renderVerbMenu(text) {
  const q = verbQuery(text);
  if (q === null) { hideVerbMenu(); return; }

  const all = await loadVerbs();
  const matches = all.filter((v) => v.name.startsWith(q));
  const el = verbMenuEl();
  if (!el || matches.length === 0) { hideVerbMenu(); return; }

  if (verbActive < 0 || verbActive >= matches.length) verbActive = 0;
  el.innerHTML = matches
    .map((v, i) => `<div class="verb-item${i === verbActive ? " active" : ""}" data-verb="${esc(v.verb)}">
        <span class="verb-name">${esc(v.verb)}</span>
        <span class="verb-help">${esc(v.help)}</span>
      </div>`)
    .join("");
  el.hidden = false;

  el.querySelectorAll(".verb-item").forEach((item) => {
    item.addEventListener("mousedown", (e) => {
      e.preventDefault(); // keep focus in the textarea
      insertVerb(item.dataset.verb);
    });
  });
}

function insertVerb(v) {
  const ta = $("#chat-text");
  if (!ta) return;
  ta.value = v + " ";
  hideVerbMenu();
  ta.focus();
}

// handleVerbKey gives the menu the arrow keys and Enter while it is open, and
// returns true when it consumed the event so the form does not also submit.
function handleVerbKey(e) {
  if (!verbMenuOpen()) return false;
  const items = $$("#verb-menu .verb-item");
  if (items.length === 0) return false;

  if (e.key === "Escape") { hideVerbMenu(); return true; }
  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    verbActive = (verbActive + (e.key === "ArrowDown" ? 1 : items.length - 1)) % items.length;
    items.forEach((it, i) => it.classList.toggle("active", i === verbActive));
    return true;
  }
  if (e.key === "Enter" || e.key === "Tab") {
    const pick = items[verbActive] || items[0];
    insertVerb(pick.dataset.verb);
    return true;
  }
  return false;
}

function attachVerbPicker() {
  const ta = $("#chat-text");
  if (!ta) return;
  ta.addEventListener("input", () => renderVerbMenu(ta.value));
  ta.addEventListener("blur", () => setTimeout(hideVerbMenu, 120));
}
