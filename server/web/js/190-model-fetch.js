/* 190-model-fetch.js — extracted from app.js (lines 4267-4336) — DYNAMIC MODEL FETCHING
 *
 * Fixed (F6): fetched models are now ENRICHED against the provider catalogue
 * so the model-detail panel shows real tier / context window / pricing instead
 * of "Dynamically fetched model" with no metadata.
 *
 * Fixed (F7): dropped the noisy auto-fetch-on-provider-change for LOCAL
 * providers (it toasted "Fetch failed" whenever Ollama/LM Studio weren't
 * running). Auto-fetch now happens only when an API key is actually present
 * in the form (see onProviderChange in 130-config.js).
 */
"use strict";

let dynamicallyFetchedModels = [];   // [{id, name, context_window, input_price, output_price, tier, description}]
let dynamicallyFetchedRaw = [];      // raw id strings (kept for back-compat with onModelChange)

// enrichFetchedModels cross-references a list of raw model ids against the
// provider catalogue so the detail panel + registered-model card can show
// real tier/context/price for known models, and a graceful fallback for
// custom/unknown ids.
function enrichFetchedModels(providerId, ids) {
  const prov = (typeof providerCatalog !== "undefined") ? providerCatalog.find((p) => p.id === providerId) : null;
  const cat = prov ? prov.models : [];
  const byId = new Map(cat.map((m) => [m.id, m]));
  return ids.map((id) => {
    const m = byId.get(id);
    if (m) {
      return {
        id, name: m.name || id,
        context_window: m.context_window || 0,
        input_price: m.input_price || 0, output_price: m.output_price || 0,
        tier: m.tier || "general", description: m.description || "",
      };
    }
    return {
      id, name: id, context_window: 0, input_price: 0, output_price: 0,
      tier: "general", description: "Dynamically fetched — no catalogue metadata",
    };
  });
}

async function fetchProviderModels() {
  const provider = $("#cfg-provider")?.value;
  const apiKey = $("#cfg-api-key")?.value;
  const baseURL = $("#cfg-base-url")?.value;
  const btn = $("#cfg-fetch-models-btn");

  if (!provider) return;

  if (btn) {
    btn.textContent = "🔄 Fetching...";
    btn.disabled = true;
  }

  try {
    const res = await fetch(API + "/api/models/fetch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ provider, api_key: apiKey, base_url: baseURL })
    });

    if (!res.ok) {
      const errText = await res.text();
      throw new Error(errText);
    }

    const data = await res.json();
    const rawIds = data.models || [];
    // Enrich against the catalogue (F6) so known models carry metadata.
    dynamicallyFetchedModels = enrichFetchedModels(provider, rawIds);
    dynamicallyFetchedRaw = rawIds;

    const selModel = $("#cfg-model-name");
    if (selModel && dynamicallyFetchedModels.length > 0) {
      selModel.innerHTML = dynamicallyFetchedModels.map((m) =>
        `<option value="${esc(m.id)}">${esc(m.id)}${m.context_window ? " (" + (m.context_window >= 1000000 ? (m.context_window / 1000000).toFixed(1) + "M" : Math.round(m.context_window / 1000) + "k") + " · " + (m.tier || "?") + ")" : " (Live)"}</option>`
      ).join("") + `<option value="__custom__">➕ Custom Model...</option>`;
      selModel.value = dynamicallyFetchedModels[0].id;
      // Refresh the unified custom dropdown (160-widgets.js) instead of the
      // old initCustomSelects()/convertSelectsToCustomDropdowns() pair.
      if (window.DC && window.DC.refreshSelect) window.DC.refreshSelect(selModel);
      selModel.dispatchEvent(new Event("change", { bubbles: true }));
      toast("success", `Fetched ${dynamicallyFetchedModels.length} models successfully!`);
    } else {
      toast("info", "No models returned from provider.");
    }
  } catch (err) {
    toast("error", "Fetch failed: " + err.message);
  } finally {
    if (btn) {
      btn.textContent = "🔄 Fetch Available Models";
      btn.disabled = false;
    }
  }
}

$("#chat-switch-cli")?.addEventListener("click", async () => {
  try {
    const res = await fetch(API + "/api/switch-cli", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ project_id: activeProjectId || "" })
    });
    if (res.ok) {
      $("#cli-modal")?.classList.add("active");
    } else {
      toast("error", "Failed to switch mode");
    }
  } catch (e) {
    toast("error", "Failed to contact server");
  }
});
