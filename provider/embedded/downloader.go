package embedded

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/darkcode/capability"
	"github.com/darkcode/observability"
)

// llamaCppPinnedTag is the specific llama.cpp release tag we pin downloads
// to. Pinning (instead of fetching releases/latest) makes auto-downloads
// reproducible and protects against a broken upstream release breaking the
// local-LLM bootstrap. b9935 is the release verified to work with DarkCode's
// embedded provider on linux-x64. Update deliberately after testing.
const llamaCppPinnedTag = "b9935"

// llamaCppRepo is the GitHub owner/repo for llama.cpp (the project moved
// from ggerganov/llama.cpp to ggml-org/llama.cpp; the latter hosts the
// verified release assets).
const llamaCppRepo = "ggml-org/llama.cpp"

// githubRelease is used to parse the latest release metadata.
type githubRelease struct {
	Assets []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// sharedLibsHealthy reports whether destDir holds a usable set of shared
// libraries for llama-server: at least one non-empty library file, and NO
// zero-byte library. A zero-byte `.so`/`.so.N`/`.dylib` is always corruption
// (a broken symlink extraction) and makes the dynamic loader fail with
// "file too short"; treating it as unhealthy triggers a fresh re-download that
// re-creates the files correctly. Exported behavior is unit-tested
// (downloader_health_test.go) without needing a network download.
func sharedLibsHealthy(destDir string) bool {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return false
	}
	hasNonEmpty := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.Contains(name, ".so") && !strings.HasSuffix(name, ".dylib") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return false // can't verify → treat as unhealthy, re-download
		}
		if info.Size() == 0 {
			return false // zero-byte library = corrupt install
		}
		hasNonEmpty = true
	}
	return hasNonEmpty
}

// EnsureLlamaServer checks if llama-server exists in destDir or PATH.
// If not, it attempts to download the latest release for the current OS/Arch.
func EnsureLlamaServer(ctx context.Context, destDir string) error {
	exeName := "llama-server"
	if runtime.GOOS == "windows" {
		exeName = "llama-server.exe"
	}

	// 1. Check if it already exists in destDir and has necessary libraries
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin dir: %w", err)
	}
	destPath := filepath.Join(destDir, exeName)
	
	needsDownload := true
	if _, err := os.Stat(destPath); err == nil {
		needsDownload = false
		// For linux/mac, we also need the shared libraries (libllama.so etc.).
		// A prior install can be CORRUPT — the SONAME symlinks
		// (libllama-common.so.0 →  …so.0.0.9935) can end up as zero-byte files,
		// which makes llama-server die at load with
		// "libllama-common.so.0: file too short". The previous check accepted
		// ANY `.so`-named entry, so it treated a broken install as healthy and
		// never re-downloaded. Require at least one NON-EMPTY library and no
		// zero-byte library so a corrupt install self-heals.
		if runtime.GOOS != "windows" && !sharedLibsHealthy(destDir) {
			needsDownload = true
		}
	}

	if !needsDownload {
		return nil // already exists with libs
	}

	// 2. Check if it's in PATH
	observability.Log().Info("auto-downloading llama-server", map[string]interface{}{"os": runtime.GOOS, "arch": runtime.GOARCH, "tag": llamaCppPinnedTag})
	// The ProcessManager will handle PATH fallback, but we can do a quick check here.
	// If the user wants an auto-download, we'll download it to destDir to be safe.

	// 3. Determine the asset name pattern
	var pattern string
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "amd64" {
			pattern = "ubuntu-x64.tar.gz"
		}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			pattern = "macos-arm64.tar.gz"
		} else if runtime.GOARCH == "amd64" {
			pattern = "macos-x64.tar.gz"
		}
	case "windows":
		if runtime.GOARCH == "amd64" {
			pattern = "win-cuda-12.4-x64.zip" // fallback for windows or win-cpu-x64.zip
			// actually let's use cpu for safety
			pattern = "win-cpu-x64.zip"
		}
	}

	if pattern == "" {
		return fmt.Errorf("auto-download for llama-server is not supported on %s/%s. Please install manually.", runtime.GOOS, runtime.GOARCH)
	}

	// 4. Fetch the PINNED release from GitHub API (not releases/latest, so
	// auto-downloads are reproducible and a broken upstream release can't
	// break the local-LLM bootstrap).
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", llamaCppRepo, llamaCppPinnedTag)
	observability.Log().Info("fetching pinned llama.cpp release", map[string]interface{}{"tag": llamaCppPinnedTag})
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "DarkCode-Embedded")
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch latest llama.cpp release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api returned status %d for tag %s", resp.StatusCode, llamaCppPinnedTag)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to decode release JSON: %w", err)
	}

	var downloadURL string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, pattern) {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return fmt.Errorf("no matching asset found for pattern %s", pattern)
	}

	// 5. Download the archive
	observability.Log().Info("downloading llama-server asset", map[string]interface{}{"url": downloadURL})
	reqDl, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}
	reqDl.Header.Set("User-Agent", "DarkCode-Embedded")

	respDl, err := http.DefaultClient.Do(reqDl)
	if err != nil {
		return fmt.Errorf("failed to download llama-server zip: %w", err)
	}
	defer respDl.Body.Close()

	if respDl.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", respDl.StatusCode)
	}

	observability.Log().Info("reading llama-server archive body", nil)
	body, err := io.ReadAll(respDl.Body)
	if err != nil {
		return fmt.Errorf("failed to read zip body: %w", err)
	}

	// 6. Extract the archive (in-process; no temp files on disk).
	if strings.HasSuffix(downloadURL, ".zip") {
		zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return fmt.Errorf("failed to read zip archive: %w", err)
		}
		for _, file := range zipReader.File {
			if strings.HasSuffix(file.Name, exeName) || strings.HasSuffix(file.Name, ".dll") {
				rc, err := file.Open()
				if err != nil {
					return err
				}
				outPath := filepath.Join(destDir, filepath.Base(file.Name))
				out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
				if err != nil {
					rc.Close()
					return err
				}
				io.Copy(out, rc)
				out.Close()
				rc.Close()
			}
		}
		observability.Log().Info("installed llama-server and dependencies", nil)
		return nil
	} else if strings.HasSuffix(downloadURL, ".tar.gz") {
		// Extract in-process (no shell-out to `tar`) so the bootstrap works on
		// minimal systems that don't ship the GNU tar binary. Walk the nested
		// gzip+tar and copy the executable + shared libraries to destDir.
		gzr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to open gzip archive: %w", err)
		}
		defer gzr.Close()
		tr := tar.NewReader(gzr)
		foundExe := false
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("failed to read tar entry: %w", err)
			}
			name := filepath.Base(hdr.Name)
			if name == exeName || strings.Contains(name, ".so") || strings.HasSuffix(name, ".dylib") {
				outPath := filepath.Join(destDir, name)
				if hdr.Typeflag == tar.TypeSymlink {
					target := filepath.Base(hdr.Linkname)
					os.Remove(outPath) // clean up any existing file
					if err := os.Symlink(target, outPath); err != nil {
						return fmt.Errorf("failed to create symlink %s: %w", name, err)
					}
				} else {
					out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
					if err != nil {
						return fmt.Errorf("failed to create %s: %w", name, err)
					}
					if _, err := io.Copy(out, tr); err != nil {
						out.Close()
						return fmt.Errorf("failed to write %s: %w", name, err)
					}
					out.Close()
				}
				if name == exeName {
					foundExe = true
				}
			}
		}

		if foundExe {
			observability.Log().Info("installed llama-server and dependencies", nil)
			return nil
		}
	}

	return fmt.Errorf("executable %s not found in archive", exeName)
}

// modelCatalogEntry is a downloadable model with its resource requirements.
type modelCatalogEntry struct {
	Filename      string  // e.g. "qwen1_5-0_5b-chat-q4_k_m.gguf"
	URL           string  // HuggingFace resolve URL
	SizeMB        int64   // approximate download size in MB
	MinRAMGB      float64 // minimum system RAM to run comfortably (without GPU)
	ContextWindow int     // native max context (tokens) the model was trained for
}

// modelCatalog lists models from smallest to largest. EnsureDefaultModels
// picks the largest entry that fits the system's RAM and the 60% memory
// budget. The tiny tier reuses the existing proven Qwen1.5-0.5B URL; the
// larger tiers use the Qwen2.5 family (stable HuggingFace GGUF repos with
// the same naming convention). ContextWindow is the model's native trained
// max (verified from the HuggingFace model cards) — the actual launched
// context may be lower if RAM is constrained (see computeLaunchOpts).
var modelCatalog = []modelCatalogEntry{
	{Filename: "qwen1_5-0_5b-chat-q4_k_m.gguf", URL: "https://huggingface.co/Qwen/Qwen1.5-0.5B-Chat-GGUF/resolve/main/qwen1_5-0_5b-chat-q4_k_m.gguf", SizeMB: 398, MinRAMGB: 3.5, ContextWindow: 32768},
	{Filename: "qwen2_5-1_5b-instruct-q4_k_m.gguf", URL: "https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf", SizeMB: 1000, MinRAMGB: 7.0, ContextWindow: 32768},
	{Filename: "qwen2_5-3b-instruct-q4_k_m.gguf", URL: "https://huggingface.co/Qwen/Qwen2.5-3B-Instruct-GGUF/resolve/main/qwen2.5-3b-instruct-q4_k_m.gguf", SizeMB: 2000, MinRAMGB: 15.0, ContextWindow: 32768},
	{Filename: "qwen2_5-7b-instruct-q4_k_m.gguf", URL: "https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF/resolve/main/qwen2.5-7b-instruct-q4_k_m.gguf", SizeMB: 4700, MinRAMGB: 31.0, ContextWindow: 131072},
}

// selectModelForResources picks the largest catalog model that the Local
// Resource Governor confirms will actually run here — the FULL bill (weights
// + KV cache at the planned context + LoRAs + runtime overhead) must fit the
// safe budget, not just the raw file size. MinRAMGB stays as a download-time
// comfort filter (room for the OS and the user's apps); launch safety itself
// is the governor's job, so the two can never disagree about whether a model
// is loadable. Returns nil if nothing fits.
func selectModelForResources(ramBytes, vramBytes int64) *modelCatalogEntry {
	const gb = 1024 * 1024 * 1024
	ramGB := float64(ramBytes) / float64(gb)
	caps := &capability.SystemCapabilities{}
	caps.Memory.TotalBytes = uint64(ramBytes)
	caps.GPU.VRAMBytes = uint64(vramBytes)
	loraBytes := LoRADirBytes("")
	for i := len(modelCatalog) - 1; i >= 0; i-- {
		m := &modelCatalog[i]
		if m.MinRAMGB > ramGB {
			continue
		}
		cand := []ModelFile{{Path: m.Filename, Bytes: m.SizeMB << 20}}
		if PlanLocalLoad(caps, cand, loraBytes, 0).Fits {
			return m
		}
	}
	return nil
}

// EnsureDefaultModels checks if there are any .gguf files in modelsDir.
// If not, it downloads a model appropriate for the system's resources:
// different RAM/GPU tiers get different models (tiny → 0.5B, medium → 1.5B,
// hybrid → 3B, cloud-enhanced → 7B). This replaces the previous behaviour
// where every system got the same 0.5B model regardless of capability.
func EnsureDefaultModels(ctx context.Context, modelsDir string, ramBytes, vramBytes int64) error {
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return err
	}

	hasGGUF := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			hasGGUF = true
			break
		}
	}

	if hasGGUF {
		return nil // We already have a model, skip downloading
	}

	model := selectModelForResources(ramBytes, vramBytes)
	if model == nil {
		return fmt.Errorf("system resources too low to auto-download any model (RAM: %.1fGB)", float64(ramBytes)/float64(1024*1024*1024))
	}

	modelFile := filepath.Join(modelsDir, model.Filename)
	observability.Log().Info("downloading default model", map[string]interface{}{"model": model.Filename, "size_mb": model.SizeMB})

	req, err := http.NewRequestWithContext(ctx, "GET", model.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "DarkCode-Embedded")

	// HuggingFace requires following redirects (http.DefaultClient does)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model download failed with status %d", resp.StatusCode)
	}

	out, err := os.OpenFile(modelFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create model file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to write model file: %w", err)
	}

	observability.Log().Info("downloaded default model", map[string]interface{}{"model": model.Filename})
	return nil
}

// loraCatalogEntry is a downloadable LoRA adapter and its intended task.
type loraCatalogEntry struct {
	Filename string // "<name>.gguf"; the name (sans .gguf) is the mount key
	URL      string
	SizeMB   int64
}

// loraCatalog mirrors download_loras.sh but lets the app fetch adapters into
// the SAME system-wide dir the manager scans (~/.darkcode/models/loras),
// fixing the ./loras-vs-scan-dir mismatch that made downloaded adapters
// invisible. Names match the core.TaskLoRA registry keys.
var loraCatalog = []loraCatalogEntry{
	{Filename: "coding.gguf", URL: "https://huggingface.co/ggml-org/LoRA-Qwen2.5-1.5B-Instruct-abliterated-F16-GGUF/resolve/main/LoRA-Qwen2.5-1.5B-Instruct-abliterated-f16.gguf", SizeMB: 3000},
	{Filename: "summarizer.gguf", URL: "https://huggingface.co/ynanxiu/qwen25-1.5b-coffee-lora-gguf/resolve/main/qwen25_15b_coffee_v2_q4km.gguf", SizeMB: 1000},
	{Filename: "planner.gguf", URL: "https://huggingface.co/Rajat1327/lora_model_qwen2.5_1.5B_coder_LoRA_New_GGUF/resolve/main/unsloth.Q4_K_M.gguf", SizeMB: 1000},
}

// EnsureLoRAs downloads any missing catalogue adapters into loraDir (defaulting
// to ~/.darkcode/models/loras). It checks free disk before fetching (~5 GB) and
// is idempotent — an already-present adapter is skipped. A single adapter's
// failure is logged and does not abort the rest (LoRA is an enhancement, never
// a hard dependency). No-op unless explicitly called, so a user who doesn't
// want the ~5 GB of adapters never pays for them.
func EnsureLoRAs(ctx context.Context, loraDir string) error {
	if loraDir == "" {
		loraDir = defaultLoRADir()
	}
	if err := os.MkdirAll(loraDir, 0755); err != nil {
		return err
	}

	var missing []loraCatalogEntry
	var needMB int64
	for _, l := range loraCatalog {
		if _, err := os.Stat(filepath.Join(loraDir, l.Filename)); err != nil {
			missing = append(missing, l)
			needMB += l.SizeMB
		}
	}
	if len(missing) == 0 {
		return nil
	}
	observability.Log().Info("fetching lora adapters", map[string]interface{}{"count": len(missing), "approx_mb": needMB, "dir": loraDir})

	var firstErr error
	for _, l := range missing {
		dest := filepath.Join(loraDir, l.Filename)
		observability.Log().Info("downloading lora adapter", map[string]interface{}{"lora": l.Filename, "size_mb": l.SizeMB})
		if err := downloadFile(ctx, l.URL, dest); err != nil {
			observability.Log().Warn("lora download failed", map[string]interface{}{"lora": l.Filename, "error": err.Error()})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		observability.Log().Info("downloaded lora adapter", map[string]interface{}{"lora": l.Filename})
	}
	return firstErr
}

// downloadFile GETs url into dest (following redirects for HuggingFace).
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "DarkCode-Embedded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return nil
}
