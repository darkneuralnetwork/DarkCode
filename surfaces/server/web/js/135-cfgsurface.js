/* 135-cfgsurface.js — the settings surface, rendered from the config type.
 *
 * The Settings tab hand-wrote its controls, so it showed a different subset of
 * the config than the API accepted and a different one again from the console.
 * Nothing was broken by that; the cost was that a setting could exist and be
 * reachable from nowhere. air_gap, the cost limits and the blast-radius
 * threshold were all in that category — described by the schema, editable by
 * no control anywhere in the app.
 *
 * This renders /api/config/schema, which is generated from the field
 * descriptors, so the list cannot fall behind the program. Most rows stay
 * read-only reference (deny_rules and the other list-shaped fields still
 * need a real add/remove editor, not a single input); WRITABLE_FIELDS below
 * gets a real control that PATCHes through patchSetting (130-config.js),
 * the same update_settings action every other settings control here uses.
 */

const WRITABLE_FIELDS = {
  air_gap: "bool",
  cost_limit_per_day_usd: "float",
  blast_radius_threshold: "float",
};

// wireConfigSurfaceToggle keeps the generated list behind a button. It is a
// reference view rather than something you edit, and unfolded by default it
// pushed the controls people do use off the screen.
function wireConfigSurfaceToggle() {
  const btn = $("#cfg-surface-toggle");
  const body = $("#cfg-surface-body");
  if (!btn || !body || btn.dataset.wired) return;
  btn.dataset.wired = "1";
  btn.addEventListener("click", async () => {
    const showing = !body.hidden;
    body.hidden = showing;
    btn.textContent = showing ? "Show all settings" : "Hide all settings";
    btn.setAttribute("aria-expanded", String(!showing));
    // Fetch on first open rather than on every config load: nothing needs the
    // list until someone asks to see it.
    if (!showing && !host_rendered) await renderConfigSurface();
  });
}

let host_rendered = false;

async function renderConfigSurface() {
  const host = $("#cfg-surface");
  if (!host) return;
  host_rendered = true;
  let data;
  try {
    const res = await fetch(API + "/api/config/schema");
    data = await res.json();
  } catch {
    host.innerHTML = `<div style="color:var(--text-mute); font-size:12px;">Could not load the settings surface.</div>`;
    return;
  }
  const fields = Array.isArray(data.fields) ? data.fields : [];
  const values = data.values || {};
  if (fields.length === 0) { host.innerHTML = ""; return; }

  const tiers = [
    { tier: "primary",  title: "Asked",    note: "the settings you actually have to decide" },
    { tier: "advanced", title: "Advanced", note: "overrides; edit in the config file" },
    { tier: "derived",  title: "Derived",  note: "computed, not set" },
  ];

  let html = "";
  for (const t of tiers) {
    const inTier = fields.filter((f) => f.tier === t.tier);
    if (inTier.length === 0) continue;
    html += `<div class="cfg-surface-tier">
      <div class="cfg-surface-tier-head">${esc(t.title)}
        <span class="cfg-surface-note">${esc(t.note)} · ${inTier.length}</span></div>`;
    let group = "";
    for (const f of inTier) {
      if (f.group !== group) {
        group = f.group;
        html += `<div class="cfg-surface-group">${esc(group)}</div>`;
      }
      html += `<div class="cfg-surface-row" title="${esc(f.help || "")}">
        <span class="cfg-surface-label">${esc(f.label)}</span>
        <code class="cfg-surface-name">${esc(f.name)}</code>
        ${renderCfgSurfaceValue(f, values[f.name])}
      </div>`;
    }
    html += `</div>`;
  }
  host.innerHTML = html;
  wireCfgSurfaceEditors(host);

  const count = $("#cfg-surface-count");
  if (count) count.textContent = `${data.primary_count} asked, ${fields.length} in total.`;
}

// renderCfgSurfaceValue renders the read-only span for every field, except
// the handful in WRITABLE_FIELDS, which get a real input instead.
function renderCfgSurfaceValue(f, value) {
  const kind = WRITABLE_FIELDS[f.name];
  if (kind === "bool") {
    return `<label class="cfg-surface-toggle">
        <input type="checkbox" data-cfg-field="${esc(f.name)}" data-cfg-kind="bool" ${value ? "checked" : ""}>
      </label>`;
  }
  if (kind === "float") {
    const v = (value === undefined || value === null) ? "" : value;
    return `<input type="number" step="any" class="glass-input cfg-surface-input"
        data-cfg-field="${esc(f.name)}" data-cfg-kind="float" value="${esc(v)}">`;
  }
  return `<span class="cfg-surface-value">${esc(formatCfgValue(value, f))}</span>`;
}

// wireCfgSurfaceEditors binds the writable rows to patchSetting (130-config.js)
// — the same update_settings action every other settings control uses.
function wireCfgSurfaceEditors(host) {
  host.querySelectorAll("[data-cfg-field]").forEach((el) => {
    el.addEventListener("change", async () => {
      const field = el.dataset.cfgField;
      const kind = el.dataset.cfgKind;
      const value = kind === "bool" ? el.checked : Number(el.value);
      if (kind === "float" && Number.isNaN(value)) {
        toast("error", `${field}: not a number`);
        return;
      }
      try {
        await patchSetting(field, value);
        toast("success", `✓ ${field} updated`);
      } catch (err) {
        toast("error", "Error: " + err.message);
      }
    });
  });
}

// formatCfgValue shows an unset value as a dash. Absence should be visible
// rather than inferred from an empty cell.
function formatCfgValue(v, f) {
  if (v === undefined || v === null || v === "") return "—";
  if (typeof v === "boolean") return v ? "on" : "off";
  if (Array.isArray(v)) return v.length ? v.join(", ") : "—";
  if (typeof v === "object") {
    const n = Object.keys(v).length;
    return n ? `${n} configured` : "—";
  }
  return String(v);
}
