/* 135-cfgsurface.js — the settings surface, rendered from the config type.
 *
 * The Settings tab hand-wrote its controls, so it showed a different subset of
 * the config than the API accepted and a different one again from the console.
 * Nothing was broken by that; the cost was that a setting could exist and be
 * reachable from nowhere. air_gap, the cost limits, the deny rules and the
 * blast-radius threshold were all in that category.
 *
 * This renders /api/config/schema, which is generated from the field
 * descriptors, so the list cannot fall behind the program.
 */

async function renderConfigSurface() {
  const host = $("#cfg-surface");
  if (!host) return;
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
        <span class="cfg-surface-value">${esc(formatCfgValue(values[f.name], f))}</span>
      </div>`;
    }
    html += `</div>`;
  }
  host.innerHTML = html;

  const count = $("#cfg-surface-count");
  if (count) count.textContent = `${data.primary_count} asked · ${fields.length} total`;
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
