/* 180-dirpicker.js — extracted from app.js (lines 4153-4266) */
// DIRECTORY PICKER
// ════════════════════════════════════════════════════════════════════════
let dirPickerTargetInput = null;
let dirPickerCurrentPath = "";

function openDirPicker(targetInputId) {
  dirPickerTargetInput = "#" + targetInputId;
  const inputVal = $(dirPickerTargetInput)?.value || "";
  const modal = $("#dir-picker-modal");
  if (modal) modal.classList.add("active");
  $("#dir-picker-new-row").style.display = "none";
  loadDirPickerContents(inputVal);
}

function closeDirPicker() {
  const modal = $("#dir-picker-modal");
  if (modal) modal.classList.remove("active");
  dirPickerTargetInput = null;
}

async function loadDirPickerContents(pathStr) {
  const list = $("#dir-picker-list");
  if (list) list.innerHTML = `<div style="padding:15px; color:var(--text-mute);">Loading...</div>`;
  
  try {
    const query = pathStr ? "?path=" + encodeURIComponent(pathStr) : "";
    const res = await fetch(API + "/api/fs/browse" + query);
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(errText);
    }
    const data = await res.json();
    dirPickerCurrentPath = data.cwd;
    renderDirPicker(data);
  } catch (err) {
    if (list) list.innerHTML = `<div style="padding:15px; color:var(--alert-red);">Error: ${err.message}</div>`;
  }
}

function renderDirPicker(data) {
  // Render Breadcrumbs
  const bcContainer = $("#dir-picker-breadcrumbs");
  if (bcContainer) {
    if (data.cwd === "/") {
      bcContainer.innerHTML = `<span class="breadcrumb" data-path="/">/</span>`;
    } else {
      const parts = data.cwd.split("/").filter(Boolean);
      let html = `<span class="breadcrumb" data-path="/">/</span>`;
      let currentPath = "";
      parts.forEach((p, i) => {
        currentPath += "/" + p;
        html += `<span class="separator">/</span><span class="breadcrumb" data-path="${esc(currentPath)}">${esc(p)}</span>`;
      });
      bcContainer.innerHTML = html;
    }
  }

  // Render List
  const list = $("#dir-picker-list");
  if (!list) return;
  
  let html = "";
  if (data.parent) {
    html += `
      <div class="dir-picker-item" data-path="${esc(data.parent)}">
        <span class="icon">📁</span>
        <span class="name">..</span>
      </div>`;
  }
  
  if (data.dirs && data.dirs.length > 0) {
    data.dirs.forEach(d => {
      html += `
        <div class="dir-picker-item" data-path="${esc(d.path)}">
          <span class="icon">📁</span>
          <span class="name">${esc(d.name)}</span>
        </div>`;
    });
  } else if (!data.parent) {
    html += `<div style="padding:15px; color:var(--text-mute);">No subdirectories found.</div>`;
  }
  
  list.innerHTML = html;
}

async function createNewDir() {
  const name = $("#dir-picker-new-input")?.value.trim();
  if (!name) return;
  
  const targetPath = dirPickerCurrentPath === "/" ? "/" + name : dirPickerCurrentPath + "/" + name;
  
  try {
    const res = await fetch(API + "/api/fs/mkdir", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: targetPath })
    });
    
    if (!res.ok) {
      const d = await res.json().catch(()=>({}));
      throw new Error(d.error || "HTTP " + res.status);
    }
    
    $("#dir-picker-new-row").style.display = "none";
    $("#dir-picker-new-input").value = "";
    toast("success", "Folder created");
    loadDirPickerContents(dirPickerCurrentPath); // refresh current view
    
  } catch (err) {
    toast("error", "Failed to create folder: " + err.message);
  }
}

// ════════════════════════════════════════════════════════════════════════
