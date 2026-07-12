package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	cwd := s.ActiveWorkspace()

	const maxDepth = 2
	const maxEntries = 5000 // bound memory/CPU on huge directories (P2)

	var entries []fileEntry
	truncated := false
	var walk func(dir, relPrefix string, depth int)
	walk = func(dir, relPrefix string, depth int) {
		if len(entries) >= maxEntries {
			truncated = true
			return
		}
		infos, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, info := range infos {
			if len(entries) >= maxEntries {
				truncated = true
				return
			}
			name := info.Name()
			if skipDirs[name] {
				continue
			}
			// Skip hidden entries except .config (project-relevant).
			if strings.HasPrefix(name, ".") && name != ".config" {
				continue
			}
			rel := name
			if relPrefix != "" {
				rel = relPrefix + "/" + name
			}
			fi, err := info.Info()
			if err != nil {
				continue
			}
			isDir := info.IsDir()
			ext := ""
			if !isDir {
				ext = filepath.Ext(name)
			}
			entries = append(entries, fileEntry{
				Name:    name,
				Path:    rel,
				Size:    fi.Size(),
				ModTime: fi.ModTime().Unix(),
				ModStr:  fi.ModTime().Format(time.RFC3339),
				IsDir:   isDir,
				Ext:     ext,
			})
			if isDir && depth < maxDepth {
				walk(filepath.Join(dir, name), rel, depth+1)
			}
		}
	}

	walk(cwd, "", 0)

	// Sort: directories first, then alphabetically.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cwd":       cwd,
		"entries":   entries,
		"count":     len(entries),
		"truncated": truncated,
	})
}

// handleFilesRead returns the content of a single workspace file for the chat
// console preview pane. Path traversal outside the cwd is rejected.
func (s *Server) handleFilesRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	cwd := s.ActiveWorkspace()
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	// Resolve and guard against path traversal.
	abs := filepath.Join(cwd, rel)
	if !strings.HasPrefix(abs+string(filepath.Separator), cwd+string(filepath.Separator)) && abs != cwd {
		writeError(w, http.StatusForbidden, "path outside workspace")
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Detect binary content (null byte).
	isBinary := false
	for _, b := range data {
		if b == 0 {
			isBinary = true
			break
		}
	}
	content := string(data)
	if isBinary {
		content = "[binary file — preview not available]"
	}
	if len(content) > 200000 {
		content = content[:200000] + "\n\n... [truncated at 200KB]"
	}
	modStr := ""
	if fi, err := os.Stat(abs); err == nil {
		modStr = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":      rel,
		"content":   content,
		"size":      len(data),
		"is_binary": isBinary,
		"mod_time":  modStr,
	})
}

// ──────────────────────────────────────────────────────────────────────────
// WORKSPACE MANAGEMENT
//
// The "active workspace" is the directory the chat console's file explorer
// browses. It defaults to the process cwd and is switched to a project's
// path when that project is activated (so the GUI follows the project the
// user is working on). It is exposed via GET/POST /api/workspace.
// ──────────────────────────────────────────────────────────────────────────

// ActiveWorkspace returns the directory the chat console should browse. It
// defaults to the process working directory until a project (or an explicit
// /api/workspace call) changes it.
func (s *Server) ActiveWorkspace() string {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	if s.activeWorkspace != "" {
		return s.activeWorkspace
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// setActiveWorkspace validates path (must be an existing directory) and
// records it as the active workspace. An empty path clears the override and
// reverts to the process cwd.
func (s *Server) setActiveWorkspace(path string) error {
	if strings.TrimSpace(path) == "" {
		s.wsMu.Lock()
		s.activeWorkspace = ""
		s.wsMu.Unlock()
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("workspace path not accessible: %s", abs)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace path is not a directory: %s", abs)
	}
	s.wsMu.Lock()
	s.activeWorkspace = abs
	s.wsMu.Unlock()
	return nil
}

// handleWorkspace exposes GET (current workspace) and POST (switch workspace).
// A POST with an empty path reverts to the process cwd.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"path":    s.ActiveWorkspace(),
			"project": s.activeProjectID(),
		})
	case http.MethodPost:
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := s.setActiveWorkspace(req.Path); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"path":    s.ActiveWorkspace(),
			"project": s.activeProjectID(),
			"ok":      true,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleWorkspaceBrowse lists a single directory level (files + subdirs) for
// the chat console's @-attachment picker. Query: ?path=<relative-or-abs>.
// An empty path lists the workspace root.
func (s *Server) handleWorkspaceBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	cwd := s.ActiveWorkspace()
	rel := r.URL.Query().Get("path")
	target := cwd
	if rel != "" {
		if filepath.IsAbs(rel) {
			target = filepath.Clean(rel)
		} else {
			target = filepath.Clean(filepath.Join(cwd, rel))
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		writeError(w, http.StatusNotFound, "path not found: "+rel)
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"cwd":     target,
			"parent":  parentDir(target),
			"entries": []fileEntry{{Name: filepath.Base(target), Path: rel, Size: info.Size(), ModTime: info.ModTime().Unix(), ModStr: info.ModTime().Format(time.RFC3339), IsDir: false, Ext: filepath.Ext(target)}},
			"count":   1,
		})
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read dir: "+err.Error())
		return
	}
	var out []fileEntry
	for _, e := range entries {
		name := e.Name()
		if skipDirs[name] {
			continue
		}
		if strings.HasPrefix(name, ".") && name != ".config" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		childRel := name
		if rel != "" {
			childRel = strings.TrimPrefix(filepath.Join(rel, name), "/")
		}
		out = append(out, fileEntry{
			Name:    name,
			Path:    childRel,
			Size:    fi.Size(),
			ModTime: fi.ModTime().Unix(),
			ModStr:  fi.ModTime().Format(time.RFC3339),
			IsDir:   e.IsDir(),
			Ext:     extOr(e.IsDir(), name),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cwd":     target,
		"parent":  parentDir(target),
		"entries": out,
		"count":   len(out),
	})
}

func (s *Server) handleFSBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	target := r.URL.Query().Get("path")
	if target == "" {
		// Default to user home, fallback to cwd, fallback to /
		if home, err := os.UserHomeDir(); err == nil {
			target = home
		} else if cwd, err := os.Getwd(); err == nil {
			target = cwd
		} else {
			target = "/"
		}
	}
	target = filepath.Clean(target)

	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, "directory not found: "+target)
		return
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read dir: "+err.Error())
		return
	}

	type dirEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	const maxDirs = 5000
	var dirs []dirEntry
	truncated := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip hidden directories
		if strings.HasPrefix(name, ".") {
			continue
		}
		if len(dirs) >= maxDirs {
			truncated = true
			break
		}
		dirs = append(dirs, dirEntry{
			Name: name,
			Path: filepath.Join(target, name),
		})
	}

	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Name < dirs[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cwd":       target,
		"parent":    parentDir(target),
		"dirs":      dirs,
		"count":     len(dirs),
		"truncated": truncated,
	})
}

// handleFSMkdir creates a new directory at the specified absolute path.
func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	target := filepath.Clean(req.Path)

	// Don't allow creating root-level dirs or paths that look dangerous.
	if target == "/" || target == "." {
		writeError(w, http.StatusBadRequest, "cannot create directory at root")
		return
	}

	if err := os.MkdirAll(target, 0755); err != nil {
		writeError(w, http.StatusInternalServerError, "mkdir: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"path":    target,
		"message": "Directory created",
	})
}

// handleReset clears short-term memory and resets session permissions.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.memSystem != nil {
		s.memSystem.STMClear()
	}
	if s.kernel != nil && s.kernel.Gate() != nil {
		s.kernel.Gate().ResetSession()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "session reset",
	})
}
