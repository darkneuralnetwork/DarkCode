/* 130-config.js — extracted from app.js (lines 2698-3094) */
// CONFIG — Provider-driven model management
// ════════════════════════════════════════════════════════════════════════
// (F-bug) `modelInput` was an undeclared free variable in the original
// app.js — every call to onProviderChange threw ReferenceError: modelInput
// is not defined (silently swallowed by init's Promise.allSettled). It was a
// leftover from an earlier design that had a free-text <input> for the model
// id; the current UI uses the #cfg-model-name <select> instead. Declared
// null here so the `if (modelInput)` guards below short-circuit cleanly.
let modelInput = null;

async function loadProviders() {
  try {
    const res = await fetch(API + "/api/providers");
    const data = await res.json();
    providerCatalog = data.providers || [];
    const sel = $("#cfg-provider");
    if (!sel) return;
    sel.innerHTML = providerCatalog.map(p => `<option value="${esc(p.id)}">${esc(p.name)}${p.local ? " (local)" : ""}</option>`).join("");
    renderProviderGrid();
    
    // Auto-select the first provider if none is selected
    if (providerCatalog.length > 0 && !sel.value) {
      sel.value = providerCatalog[0].id;
    }
    
    // Refresh custom select DOM
    if (window.DC && window.DC.refreshSelect) window.DC.refreshSelect(sel); else window.convertSelectsToCustomDropdowns && window.convertSelectsToCustomDropdowns();
    sel.dispatchEvent(new Event("change", { bubbles: true }));
  } catch (err) {
    console.error("failed to load providers", err);
  }
}

function renderProviderGrid() {
  const grid = $("#provider-grid");
  if (!grid) return;
  grid.innerHTML = providerCatalog.map(p => `
    <div class="provider-card" data-pid="${esc(p.id)}">
      <div class="provider-card-name">${esc(p.name)}</div>
      <div class="provider-card-meta">
        <span>${p.models.length} models</span>
        ${p.local ? '<span class="provider-badge-local">LOCAL</span>' : `<span class="provider-badge-auth">${esc(p.auth_scheme)}</span>`}
      </div>
    </div>`).join("");
}

// initModelsConnToggle wires the segmented toggle that merges the
// "Connection Status" and "Add Model" sub-sections into one panel (Fix H).
// Idempotent: safe to call on every loadConfig; re-binds only if the toggle
// exists and hasn't been bound yet (data-bound marker).
function initModelsConnToggle() {
  const toggle = $("#cfg-mc-toggle");
  if (!toggle || toggle.dataset.bound === "1") return;
  toggle.dataset.bound = "1";
  const setActive = (view) => {
    toggle.querySelectorAll(".cfg-mc-btn").forEach(b => {
      const on = b.dataset.view === view;
      b.classList.toggle("active", on);
      b.style.color = on ? "var(--accent-3)" : "var(--text-mute)";
      b.style.background = on ? "var(--accent-glow)" : "transparent";
      b.style.borderColor = on ? "rgba(255,145,0,0.3)" : "transparent";
    });
    const status = $("#cfg-mc-status");
    const add = $("#cfg-mc-add");
    if (status) status.style.display = view === "status" ? "block" : "none";
    if (add) add.style.display = view === "add" ? "block" : "none";
  };
  toggle.addEventListener("click", (e) => {
    const btn = e.target.closest(".cfg-mc-btn");
    if (btn) setActive(btn.dataset.view);
  });
  // Default to the connection-status view.
  setActive("status");
}

function onProviderChange() {
  const id = $("#cfg-provider").value;
  const p = providerCatalog.find(x => x.id === id);
  if (!p) return;
  dynamicallyFetchedModels = []; // Reset dynamic list on provider change
  dynamicallyFetchedRaw = [];     // (F6) keep the raw list in sync too
  const selModel = $("#cfg-model-name");
  if (selModel) {
    selModel.innerHTML = p.models.map(m => `<option value="${esc(m.id)}">${esc(m.name)}</option>`).join("") + `<option value="__custom__">➕ Custom Model...</option>`;
    if (window.DC && window.DC.refreshSelect) window.DC.refreshSelect(selModel); else window.convertSelectsToCustomDropdowns && window.convertSelectsToCustomDropdowns();
  }
  const bu = $("#cfg-base-url");

  if (p.id === "openai-compatible") {
    // Custom endpoint: the user supplies both the base URL and the model id.
    if (bu) { bu.value = ""; bu.placeholder = "https://your-endpoint/v1  (or http://localhost:8000/v1)"; }
    if (modelInput) { modelInput.value = ""; modelInput.placeholder = "enter your model id (e.g. gpt-4o-mini, llama3.1)"; }
  } else {
    if (bu) { bu.value = p.base_url; bu.placeholder = ""; }
    if (modelInput) {
      modelInput.value = p.models[0] ? p.models[0].id : "";
      modelInput.placeholder = "e.g. gpt-4o-mini, llama3.1, or your custom model id";
    }
  }
  
  // Update Get Key link
  const link = $("#cfg-get-key-link");
  if (link) {
    let url = "";
    if (id === "anthropic") url = "https://console.anthropic.com/settings/keys";
    else if (id === "openai") url = "https://platform.openai.com/api-keys";
    else if (id === "google") url = "https://aistudio.google.com/app/apikey";
    else if (id === "openrouter") url = "https://openrouter.ai/keys";
    else if (id === "xai") url = "https://console.x.ai/";
    else if (id === "deepseek") url = "https://platform.deepseek.com/";
    
    if (url && !p.local) {
      link.href = url;
      link.style.display = "block";
    } else {
      link.style.display = "none";
    }
  }
  
  // Update provider grid selection
  $$(".provider-card").forEach(c => c.classList.toggle("selected", c.dataset.pid === id));
  onModelChange();
  
  // Auto-fetch ONLY when an API key is actually present in the form (F7).
  // The previous code also auto-fetched for every local provider on every
  // selection, which toasted "Fetch failed" whenever Ollama/LM Studio
  // weren't running — noisy and misleading. Local providers expose their
  // catalogue already; the user can click "Fetch Available Models" to probe
  // a running local server explicitly.
  const apiKey = $("#cfg-api-key")?.value;
  if (apiKey && apiKey.trim() !== "") {
    fetchProviderModels();
  }
}

function onModelChange() {
  const id = $("#cfg-provider").value;
  const mid = $("#cfg-model-name").value;
  
  if (mid === "__custom__") {
    const customId = prompt("Enter custom model ID:");
    const selModel = $("#cfg-model-name");
    if (customId && customId.trim() !== "") {
      selModel.innerHTML = `<option value="${esc(customId)}">${esc(customId)}</option>` + selModel.innerHTML;
      selModel.value = customId;
    } else {
      selModel.value = selModel.options[1] ? selModel.options[1].value : "";
    }
    if (window.DC && window.DC.refreshSelect) window.DC.refreshSelect(selModel); else window.convertSelectsToCustomDropdowns && window.convertSelectsToCustomDropdowns();
    selModel.dispatchEvent(new Event("change", { bubbles: true }));
    return;
  }
  
  const p = providerCatalog.find(x => x.id === id);
  let m = p && p.models.find(x => x.id === mid);
  
  const det = $("#model-detail");
  if (!det) return;
  
  if (!m) {
    // (F6) A dynamically fetched model now carries enriched metadata
    // (tier/context/price from the catalogue) instead of a bare "unknown".
    const dyn = dynamicallyFetchedModels.find(x => x.id === mid);
    if (dyn) {
      m = dyn;
    } else if (mid) {
      // The user typed a custom model id (e.g. an OpenAI-compatible endpoint).
      // No catalogue metadata is available; show a helpful note instead of
      // leaving the detail panel blank.
      m = { tier: "general", context_window: 0, input_price: 0, output_price: 0, description: "Custom model — no catalogue metadata. It will be registered with the provider/base URL/key above." };
    } else {
      return;
    }
  }
  
  const ctxK = m.context_window >= 1000000 ? (m.context_window/1000000).toFixed(1)+"M" : 
               m.context_window > 0 ? Math.round(m.context_window/1000)+"k" : "Unknown";
               
  const price = (m.input_price === 0 && m.output_price === 0)
    ? (m.context_window === 0 ? "Pricing unknown" : "Free / local")
    : `$${m.input_price.toFixed(2)} in · $${m.output_price.toFixed(2)} out / 1M tok`;
    
  det.innerHTML = `
    <div class="md-row"><span class="md-tier">${esc(m.tier || "general")}</span><span>${esc(ctxK)} context</span></div>
    <div class="md-price">${price}</div>
    ${m.description ? `<div class="md-desc">${esc(m.description)}</div>` : ""}`;
}

// updateLoopIndicator syncs the header Agentic Loop badge to the on/off
// state. Called from loadConfig and the settings Apply button.
function updateLoopIndicator(on) {
  const ind = $("#loop-indicator");
  if (!ind) return;
  ind.hidden = false;
  ind.classList.toggle("on", !!on);
  const lbl = ind.querySelector(".loop-label");
  if (lbl) lbl.textContent = on ? "Loop: ON" : "Loop: OFF";
  ind.title = on
    ? "Agentic Loop (ReAct) is ON — the Loop chat mode is available"
    : "Agentic Loop (ReAct) is OFF — toggle in Configurations to enable the Loop chat mode";
}

// updateLoopModeOption shows/hides the "Loop Mode" entry in the chat console
// mode dropdown based on the Settings master toggle. The Loop option is only
// reachable when loop engineering is enabled, so the agent never silently runs
// the ReAct loop for users who haven't opted in. If the user disables the
// loop while Loop mode is the active selection, fall back to General so the
// next send doesn't dispatch a loop the UI no longer advertises.
function updateLoopModeOption(on) {
  const opt = $("#mode-option-loop");
  if (opt) opt.style.display = on ? "block" : "none";
  // Phase 5: gate the composer's Loop toggle on the master setting so it never
  // advertises a loop the user hasn't enabled. Disabled + unchecked when off.
  const loopToggle = $("#chat-loop-toggle");
  const loopWrap = $("#chat-loop-wrap");
  if (loopToggle) {
    loopToggle.disabled = !on;
    if (!on) loopToggle.checked = false;
  }
  if (loopWrap) {
    loopWrap.style.opacity = on ? "1" : "0.4";
    loopWrap.title = on
      ? "Loop engineering: auto-generate and run tasks"
      : "Enable Loop engineering in Settings to use this";
  }
  if (!on) {
    const modeVal = $("#chat-mode-value");
    const modeBtn = $("#chat-mode-btn");
    if (modeVal && modeVal.value === "loop") {
      // Fall back to Project (the default mode) — not General — so tools stay
      // available after the loop is disabled mid-session.
      modeVal.value = "project";
      if (modeBtn) modeBtn.title = "Chat Mode: Project Mode";
    }
  }
}

// renderExecutionProfile marks the active segment of the Execution Profile
// toggle (Auto / Sequential / Parallel) and updates the hint text. Called
// from loadConfig and the segment click handler (150-events.js).
function renderExecutionProfile(profile) {
  if (!profile) profile = "auto";
  const hints = {
    auto: "Auto — resolved per request: Sequential when only free-tier cloud models are registered (429-safe), Parallel otherwise.",
    sequential: "Sequential — one model call at a time. Safest for free-tier models with strict rate limits.",
    parallel: "Parallel — DAG sub-agents + consensus fan-out run concurrently (today's behavior). Best for paid/local models."
  };
  $$(".cfg-seg-btn").forEach(btn => {
    const active = btn.dataset.profile === profile;
    btn.style.background = active ? "var(--accent-3)" : "transparent";
    btn.style.color = active ? "#000" : "var(--text-mute)";
    btn.style.borderColor = active ? "var(--accent-3)" : "transparent";
    btn.style.fontWeight = active ? "600" : "400";
  });
  const hint = $("#cfg-exec-profile-hint");
  if (hint) hint.textContent = hints[profile] || hints.auto;
}

// renderActiveProvider populates the "Active Provider" connection-details
// panel from the /api/config response.
function renderActiveProvider(d) {
  const set = (id, val) => {
    const el = $(id);
    if (el) el.textContent = (val === undefined || val === null || val === "") ? "—" : val;
  };
  // When no cloud model is configured but the embedded LLM is running, show
  // the embedded model's details instead of empty dashes.
  const emb = d.embedded || {};
  if ((!d.model || d.model === "") && emb.is_running) {
    set("#ac-model", emb.model_id);
    set("#ac-provider", "embedded (llama.cpp)");
    set("#ac-base-url", emb.base_url);
    set("#ac-api-key", "✓ Local (no key)");
    set("#ac-auth", "none (local)");
    set("#ac-context", "4k");
    set("#ac-temp", (typeof d.temperature === "number") ? d.temperature.toFixed(2) : "—");
    set("#ac-concurrent", d.max_concurrent || "—");
    set("#ac-compress", d.compress_context ? "Enabled" : "Disabled");
    set("#ac-uimode", d.ui_mode ? "Enabled" : "Disabled");
    return;
  }
  set("#ac-model", d.model);
  set("#ac-provider", d.provider);
  set("#ac-base-url", d.base_url);
  set("#ac-api-key", d.has_api_key ? "✓ Configured" : "✗ Missing");
  // Resolve auth scheme + locality from the provider catalogue.
  let auth = "—";
  if (d.provider) {
    const p = providerCatalog.find(x => x.id === d.provider);
    if (p) auth = p.local ? "none (local)" : p.auth_scheme;
  }
  set("#ac-auth", auth);
  set("#ac-context", d.context_length ? fmtNum(d.context_length) + " tok" : "—");
  set("#ac-temp", (typeof d.temperature === "number") ? d.temperature.toFixed(2) : "—");
  set("#ac-concurrent", d.max_concurrent || "—");
  set("#ac-compress", d.compress_context ? "Enabled" : "Disabled");
  set("#ac-uimode", d.ui_mode ? "Enabled" : "Disabled");
}

async function loadConfig() {
  const c = $("#config-content");
  if (!c) return;
  try {
    const res = await fetch(API + "/api/config");
    const d = await res.json();

    if (d.routing_mode) $("#cfg-routing").value = d.routing_mode;
    if (d.safety_level) $("#cfg-safety").value = d.safety_level;
    if (d.max_turns) $("#cfg-max-turns").value = d.max_turns;

    // Agentic Loop (looping technology) — populate toggle + max loops +
    // header indicator.
    const loopOn = !!d.agentic_loop;
    const loopChk = $("#cfg-agentic-loop");
    if (loopChk) loopChk.checked = loopOn;
    const loopLbl = $("#cfg-agentic-loop-label");
    if (loopLbl) loopLbl.textContent = loopOn ? "Enabled" : "Disabled";
    const maxLoops = $("#cfg-max-loops");
    if (maxLoops) maxLoops.value = d.max_loops || 20;
    updateLoopIndicator(loopOn);
    updateLoopModeOption(loopOn);
    renderExecutionProfile(d.execution_profile);

    // Local LLM
    const localChk = $("#cfg-enable-local-llm");
    const localOffloadChk = $("#cfg-enable-local-offload");
    const localLbl = $("#cfg-enable-local-llm-label");
    const offloadLbl = $("#cfg-enable-local-offload-label");
    if (localChk && d.enable_local_llm !== undefined) {
      localChk.checked = d.enable_local_llm;
      localLbl.textContent = d.enable_local_llm ? "Auto-load ON" : "Auto-load OFF";
      localLbl.style.color = d.enable_local_llm ? "var(--text-bright)" : "var(--text-mute)";
    }
    if (localOffloadChk && d.enable_local_offloading !== undefined) {
      localOffloadChk.checked = d.enable_local_offloading;
      offloadLbl.textContent = d.enable_local_offloading ? "Offloading ON" : "Offloading OFF";
      offloadLbl.style.color = d.enable_local_offloading ? "var(--text-bright)" : "var(--text-mute)";
    }

    // Memory Profile: reflect the saved local-model context/RAM knob.
    const memProf = $("#cfg-memory-profile");
    if (memProf && d.memory_profile !== undefined) {
      memProf.value = d.memory_profile || "";
    }

    // Force Local: pin routing to the local model (no cloud fallback).
    const forceChk = $("#cfg-force-local");
    if (forceChk && d.force_local !== undefined) {
      forceChk.checked = !!d.force_local;
      renderForceLocalBadge(!!d.force_local);
    }

    // Resolve auth schemes / pricing from the provider catalogue when rendering
    // the active provider + registered model cards. It is already loaded by
    // init(); only fetch if empty (e.g. loadConfig ran before init finished).
    if (!providerCatalog || providerCatalog.length === 0) await loadProviders();

    // Active provider / connection details
    renderActiveProvider(d);

    // Local LLM status row (state / model / endpoint)
    renderLocalLLMStatus(d.embedded);

    const mList = $("#models-list");
    mList.innerHTML = "";
    const models = d.models || {};
    const keys = Object.keys(models);
    if (!keys.length) {
      mList.innerHTML = `<div class="mem-empty">No cloud models registered. Add one below, edit .config directly, or use <code style="color:var(--accent-3)">/models add</code> in the CLI.</div>`;
    } else {
      keys.forEach((k) => {
        const v = models[k];
        const isPrimary = k === d.model;
        const tier = v.tier || "coding";
        // Resolve catalogue metadata for this provider+model (if known).
        const prov = providerCatalog.find(x => x.id === v.provider);
        let catModel = null;
        if (prov) {
          catModel = prov.models.find(m => m.id === v.model);
          // Google's OpenAI-compatible API requires a "models/" prefix on the
          // id (e.g. "models/gemini-2.5-pro"); fall back to a prefix-stripped
          // match so catalogue metadata still resolves.
          if (!catModel && v.model && v.model.startsWith("models/")) {
            catModel = prov.models.find(m => m.id === v.model.slice(7));
          }
        }
        const authBadge = prov
          ? (prov.local ? '<span class="provider-badge-local">LOCAL</span>'
                        : `<span class="provider-badge-auth">${esc(prov.auth_scheme)}</span>`)
          : "";
        // Temporary enable/disable (local-first upgrade §6c) — matched against
        // the router's live registered-model list by actual model id (v.model),
        // since that's what RegisterModel was called with, not the config key k.
        const regModel = (d.registered_models || []).find(m => m.name === v.model || m.name === k);
        const isDisabled = !!(regModel && regModel.disabled);
        const untilTxt = isDisabled && regModel.disabled_until
          ? new Date(regModel.disabled_until).toLocaleTimeString()
          : "";
        const disableBtnHtml = isDisabled
          ? `<button class="btn-glow btn-xs" data-act="enable" data-model="${esc(v.model || k)}">Enable</button>`
          : `<button class="btn-glow btn-xs" data-act="disable" data-model="${esc(v.model || k)}">Disable</button>`;
        const apiKeyTxt = v.api_key
          ? v.api_key
          : (prov && prov.local ? "local" : "—");
        const ctxTxt = catModel
          ? (catModel.context_window >= 1000000
              ? (catModel.context_window / 1000000).toFixed(1) + "M"
              : Math.round(catModel.context_window / 1000) + "k")
          : "";
        const priceTxt = catModel
          ? ((catModel.input_price === 0 && catModel.output_price === 0)
              ? "Free / local"
              : `$${catModel.input_price.toFixed(2)} in · $${catModel.output_price.toFixed(2)} out`)
          : "";
        const card = document.createElement("div");
        card.className = "model-card" + (isPrimary ? " primary" : "");
        // Build the consensus-role dropdown for non-primary models. The
        // primary model is always the synthesizer (no dropdown).
        const roleOpts = ["critic","skeptic","knowledge_booster","creative","analyst","verifier","general"];
        const currentRole = v.role || "critic";
        let roleHtml;
        if (isPrimary) {
          roleHtml = `<span class="badge accent" style="font-size:11px">★ Synthesizer</span>`;
        } else {
          const opts = roleOpts.map(r =>
            `<option value="${esc(r)}"${r === currentRole ? " selected" : ""}>${esc(r.replace(/_/g," "))}</option>`).join("");
          roleHtml = `<select class="glass-input mc-role-select" data-act="role" data-model="${esc(k)}" style="width:auto;font-size:12px;padding:2px 6px">${opts}</select>`;
        }
        card.innerHTML = `
          <div class="mc-head">
            <div class="mc-name">${esc(v.model || k)} ${isPrimary ? '<span class="badge accent">★ Primary</span>' : ""} ${isDisabled ? `<span class="badge" style="color:var(--danger,#f66)" title="${untilTxt ? "Disabled until " + esc(untilTxt) : ""}">⊘ disabled</span>` : ""}</div>
            <div class="mc-actions">
              ${isPrimary ? "" : `<button class="btn-glow btn-xs" data-act="primary" data-model="${esc(k)}">Set Primary</button>`}
              <input type="text" class="glass-input mc-disable-dur" data-model="${esc(v.model || k)}" placeholder="1h" title="Disable duration (e.g. 30m, 1h, 2h)" style="width:52px;font-size:11px;padding:2px 4px" ${isDisabled ? "disabled" : ""}>
              ${disableBtnHtml}
              <button class="btn-glow btn-xs danger" data-act="remove" data-model="${esc(k)}">Remove</button>
            </div>
          </div>
          <div class="mc-meta">
            <span class="mc-tier ${esc(tier)}">${esc(tier)}</span>
            <span class="req-prov">${esc(v.provider || "unknown")}</span>
            ${authBadge}
            <span>${esc(v.base_url || "")}</span>
          </div>
          <div class="mc-meta">
            <span>key: ${esc(apiKeyTxt)}</span>
            ${ctxTxt ? `<span>ctx: ${esc(ctxTxt)}</span>` : ""}
            ${priceTxt ? `<span>${esc(priceTxt)}</span>` : ""}
            ${catModel && catModel.description ? `<span style="color:var(--text-dim)">${esc(catModel.description)}</span>` : ""}
          </div>
          <div class="mc-meta mc-role-row">
            <span style="color:var(--text-dim);font-size:12px">Consensus role:</span>
            ${roleHtml}
          </div>`;
        mList.appendChild(card);
      });
    }

    // Render the embedded/local model as a virtual card if it's running.
    // This card supports "Set Primary" and consensus-role assignment via the
    // same #models-list event delegation as cloud model cards — but has no
    // "Remove" button (local models are toggled, not removed).
    renderEmbeddedModelCard(d.embedded, d.model, d.registered_models || []);

    if (d.max_turns) $("#cfg-max-turns").value = d.max_turns;

    // Populate the compressor-model dropdown with all registered models +
    // the "(primary model)" default option, then select the saved value.
    const compSel = $("#cfg-compressor-model");
    if (compSel) {
      let compOpts = `<option value="">(primary model)</option>`;
      const compKeys = Object.keys(d.models || {});
      compKeys.forEach(k => {
        const label = (k === d.model) ? `${esc(k)} (primary)` : esc(k);
        compOpts += `<option value="${esc(k)}"${k === d.compressor_model ? " selected" : ""}>${label}</option>`;
      });
      compSel.innerHTML = compOpts;
      compSel.value = d.compressor_model || "";
    }

    c.dataset.loaded = "1";

    // Update model switcher in topbar
    updateModelSwitcher(d);
    // Wire the merged Models & Connection toggle (Fix H).
    initModelsConnToggle();
  } catch (err) {
    console.error("Failed to load config:", err);
  }
}

// renderForceLocalBadge toggles the "LOCAL ONLY" badge and the Force Local
// switch label to reflect whether routing is currently pinned to the local
// model. Called on load and after the Apply POST returns force_local.
function renderForceLocalBadge(active) {
  const badge = $("#cfg-force-local-badge");
  if (badge) badge.style.display = active ? "inline-block" : "none";
  const lbl = $("#cfg-force-local-label");
  if (lbl) {
    lbl.textContent = active ? "Force Local ON" : "Force Local";
    lbl.style.color = active ? "var(--text-bright)" : "var(--text-mute)";
  }
}

// renderLocalLLMStatus populates the Local LLM panel's status row (state /
// model / endpoint) from the /api/config embedded field. Hidden when the
// local LLM is not enabled or not running.
function renderLocalLLMStatus(emb) {
  const row = $("#cfg-local-llm-status");
  if (!row) return;
  if (!emb || !emb.is_running) {
    row.style.display = "none";
    return;
  }
  row.style.display = "flex";
  const set = (id, val) => { const el = $(id); if (el) el.textContent = val || "—"; };
  set("#cfg-local-llm-state", emb.state || "running");
  set("#cfg-local-llm-model", emb.model_id || "—");
  set("#cfg-local-llm-url", emb.base_url || "—");
}

// renderEmbeddedModelCard renders a model card for the running local/embedded
// model in the "Registered Models" list. It supports "Set Primary" and the
// consensus-role dropdown via the existing #models-list event delegation —
// but has no "Remove" button (local models are toggled via the Local LLM
// panel, not removed from the config).
function renderEmbeddedModelCard(emb, primaryModel, registeredModels) {
  const mList = $("#models-list");
  if (!mList || !emb || !emb.is_running || !emb.model_id) return;
  const isPrimary = emb.is_primary || primaryModel === "";
  // Look up the actual tier from the router's registered-models list so the
  // CSS class matches (.mc-tier.coding / .mc-tier.reasoning / .mc-tier.local).
  let tier = "local";
  for (const m of (registeredModels || [])) {
    if (m.name === emb.model_id) { tier = m.tier || "local"; break; }
  }
  const roleOpts = ["critic","skeptic","knowledge_booster","creative","analyst","verifier","general"];
  const currentRole = emb.role || "critic";
  let roleHtml;
  if (isPrimary) {
    roleHtml = `<span class="badge accent" style="font-size:11px">★ Synthesizer</span>`;
  } else {
    const opts = roleOpts.map(r => {
      const sel = r === currentRole ? " selected" : "";
      return `<option value="${esc(r)}"${sel}>${esc(r.replace(/_/g," "))}</option>`;
    }).join("");
    roleHtml = `<select class="glass-input mc-role-select" data-act="role" data-model="${esc(emb.model_id)}" style="width:auto;font-size:12px;padding:2px 6px">${opts}</select>`;
  }
  const card = document.createElement("div");
  card.className = "model-card" + (isPrimary ? " primary" : "");
  card.innerHTML = `
    <div class="mc-head">
      <div class="mc-name">${esc(emb.model_id)} ${isPrimary ? '<span class="badge accent">★ Primary</span>' : ''}</div>
      <div class="mc-actions">
        ${isPrimary ? '' : `<button class="btn-glow btn-xs" data-act="primary" data-model="${esc(emb.model_id)}">Set Primary</button>`}
      </div>
    </div>
    <div class="mc-meta">
      <span class="mc-tier ${esc(tier)}">${esc(tier)}</span>
      <span class="req-prov">embedded</span>
      <span class="provider-badge-local">LOCAL</span>
      <span>${esc(emb.base_url || "")}</span>
    </div>
    <div class="mc-meta">
      <span>key: local</span>
      <span style="color:var(--green)">Free / offline</span>
    </div>
    <div class="mc-meta mc-role-row">
      <span style="color:var(--text-dim);font-size:12px">Consensus role:</span>
      ${roleHtml}
    </div>`;
  mList.appendChild(card);
}

function updateModelSwitcher(cfg) {
  const sel = $("#model-select");
  if (!sel) return;
  const models = cfg.models || {};
  const keys = Object.keys(models);
  const emb = cfg.embedded || {};
  const registered = cfg.registered_models || [];

  // D3: Build the option list from registered_models (the kernel's actual
  // runtime state) as the source of truth. This keeps the dropdown in sync
  // with what the router really has — previously it only used cfg.models +
  // cfg.embedded, which could drift (e.g. a model registered at runtime that
  // isn't in the config map). Fall back to the legacy path only when the
  // kernel hasn't registered anything yet (still booting).
  //
  // Dedup by model name + group local vs cloud with <optgroup> for
  // readability. Labels get a provider icon (🖥️ local / ☁️ cloud) and a
  // (primary) tag on the active primary.

  let localOpts = "";
  let cloudOpts = "";
  const seen = {}; // dedup by model name

  function modelLabel(name, isPrimary) {
    const isLocal = name.startsWith("embedded/");
    const icon = isLocal ? "🖥️" : "☁️";
    const display = isLocal ? name.replace("embedded/", "") : name;
    const tag = isPrimary ? " (primary)" : "";
    return icon + " " + display + tag;
  }

  function addOption(name, isPrimary) {
    if (seen[name]) return;
    seen[name] = true;
    const opt = `<option value="${esc(name)}">${esc(modelLabel(name, isPrimary))}</option>`;
    if (name.startsWith("embedded/")) localOpts += opt;
    else cloudOpts += opt;
  }

  if (registered.length > 0) {
    registered.forEach((m) => addOption(m.name, m.is_primary));
  } else {
    // Fallback: legacy path (embedded + config map) for the still-booting case.
    if (emb.is_running && emb.model_id) addOption(emb.model_id, emb.is_primary);
    if (cfg.model) addOption(cfg.model, true);
    keys.forEach((k) => addOption(k, k === cfg.model));
  }

  let opts = "";
  if (localOpts) opts += `<optgroup label="Local">${localOpts}</optgroup>`;
  if (cloudOpts) opts += `<optgroup label="Cloud">${cloudOpts}</optgroup>`;
  if (!opts) opts = `<option value="">No models registered</option>`;
  sel.innerHTML = opts;

  // Select the active primary.
  let primaryName = "";
  if (registered.length > 0) {
    const p = registered.find((m) => m.is_primary);
    if (p) primaryName = p.name;
  }
  if (!primaryName) {
    if (cfg.model) primaryName = cfg.model;
    else if (emb.is_running && emb.model_id) primaryName = emb.model_id;
    else if (keys.length > 0) primaryName = keys[0];
  }
  sel.value = primaryName;
}

async function configAction(action, modelName) {
  try {
    const res = await fetch(API + "/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, model_name: modelName })
    });
    if (!res.ok) throw new Error(await res.text());
    await loadConfig();
    if (action === "set_primary") toast("success", `✓ Primary model switched to ${modelName} (hot-reloaded)`);
    else if (action === "remove_model") toast("info", `✓ Removed model ${modelName}`);
  } catch (err) {
    toast("error", "Error: " + err.message);
  }
}

// modelDisable / modelEnable — GUI counterpart of the CLI's "/models disable"
// and "/models enable" (local-first upgrade §6c). Hits the dedicated
// /api/models/disable|enable endpoints (not /api/config — this is a live
// router-only toggle, not a persisted config change) then reloads the card
// list so the ⊘ badge and button state reflect the new status.
async function modelDisable(modelName, duration) {
  try {
    const body = { model: modelName };
    if (duration) body.duration = duration;
    const res = await fetch(API + "/api/models/disable", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    if (!res.ok) throw new Error(await res.text());
    await loadConfig();
    toast("success", `✓ Disabled ${modelName}${duration ? " for " + duration : " for 1h"}`);
  } catch (err) {
    toast("error", "Error: " + err.message);
  }
}

async function modelEnable(modelName) {
  try {
    const res = await fetch(API + "/api/models/enable", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: modelName })
    });
    if (!res.ok) throw new Error(await res.text());
    await loadConfig();
    toast("success", `✓ Re-enabled ${modelName}`);
  } catch (err) {
    toast("error", "Error: " + err.message);
  }
}

// configActionRole sets the consensus role for a non-primary model. Unlike
// configAction, it does NOT call loadConfig() afterward (which would re-render
// the cards and reset the dropdown focus); instead it just shows a toast. The
// next loadConfig() call (e.g. when switching to the config tab) will reflect
// the saved role.
async function configActionRole(modelName, role) {
  try {
    const res = await fetch(API + "/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "set_role", model_name: modelName, role })
    });
    if (!res.ok) throw new Error(await res.text());
    toast("success", `✓ ${modelName} role set to ${role}`);
  } catch (err) {
    toast("error", "Error: " + err.message);
  }
}

// ════════════════════════════════════════════════════════════════════════
