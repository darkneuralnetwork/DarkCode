package embedded

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkcode/capability"
)

// governor.go — the Local Resource Governor: the single decision point for
// whether and how the local llama-server may be launched. It owns EVERY byte
// the process will consume — model weights + KV cache (grows with the context
// window) + pre-loaded LoRA adapters + a fixed runtime overhead — and checks
// the sum against the machine's FREE memory. Previously three independent
// checks (downloader model-size budget, loadLocalLLM's 60% cap, and
// computeLaunchOpts' RAM-tiered context) each looked at one slice of the cost
// and none saw the total, so model+KV could jointly exceed RAM and swap-thrash
// the machine. All three now consume one LoadPlan; their private arithmetic is
// gone, so no second opinion can disagree.

// LoadPlan is the governor's verdict: which model to run, with what context
// window and parallel slots, and the full memory bill — or a refusal with a
// human-readable reason. A refusal is surfaced (log, /local, GUI status), not
// silently swallowed: "local disabled: needs 9.8 GB, 5.2 GB free" beats a
// frozen machine.
type LoadPlan struct {
	ModelPath  string
	ModelBytes int64
	KVBytes    int64 // f(nCtx, model architecture, q8_0 KV quant)
	LoRABytes  int64 // adapters pre-loaded at launch (scale 0, still resident)
	NCtx       int   // -c value to launch with
	NParallel  int   // -np value; effective per-request window = NCtx/NParallel
	Fits       bool
	Refusal    string // set when !Fits
}

// EffectiveWindow is the usable per-request context in tokens. llama-server
// splits -c across -np decode slots, so the window a single request actually
// gets is NCtx/NParallel — the silent quartering behind most "context window
// exceeded" errors when np defaulted to 4.
func (p LoadPlan) EffectiveWindow() int {
	if p.NParallel <= 0 {
		return p.NCtx
	}
	return p.NCtx / p.NParallel
}

// ModelFile is a load candidate handed to the governor — either a real .gguf
// on disk (loadLocalLLM) or a catalogue entry not yet downloaded
// (EnsureDefaultModels), which is why it carries size explicitly instead of
// stat-ing the path.
type ModelFile struct {
	Path  string
	Bytes int64
}

const (
	// governorSafeFraction of free memory is the ceiling for the whole
	// llama-server bill. The remainder is headroom for the Go process, page
	// cache, and the user's other applications — the difference between
	// "model runs" and "machine swaps".
	governorSafeFraction = 0.55
	// governorOverheadBytes covers the llama-server process itself: compute
	// buffers, activations, tokenizer, HTTP server. Measured ~300-450MB on
	// the shipped models; 512MB keeps the estimate on the safe side.
	governorOverheadBytes = 512 << 20
	// governorCtxFloor is the smallest context the ladder will offer. Below
	// 4096 total (2048 effective at np=2) the model is too cramped for the
	// system prompt + any real task, so refusal is more honest.
	governorCtxFloor = 4096
	// governorFallbackKVPerTok is the q8_0 KV-cache bytes/token estimate for
	// GGUFs not in the catalogue — set at the catalogue's worst case (the
	// MHA 0.5B) so unknown models are budgeted pessimistically, never
	// optimistically.
	governorFallbackKVPerTok = 52224
)

// kvBytesPerCtxToken returns the q8_0 KV-cache cost per context token for a
// known catalogue model, derived from its architecture:
// 2 (K+V) × n_layers × (n_kv_heads × head_dim) × ~1.0625 bytes (q8_0).
// Table-driven so the estimate is deterministic and testable; unknown files
// get the pessimistic fallback.
func kvBytesPerCtxToken(filename string) int64 {
	name := strings.ToLower(filepath.Base(filename))
	switch {
	case strings.Contains(name, "qwen1_5-0_5b"):
		return 52224 // 24 layers × 1024 kv_dim (MHA, no GQA)
	case strings.Contains(name, "qwen2_5-1_5b"):
		return 15232 // 28 layers × 256 kv_dim (GQA)
	case strings.Contains(name, "qwen2_5-3b"):
		return 19584 // 36 layers × 256 kv_dim (GQA)
	case strings.Contains(name, "qwen2_5-7b"):
		return 30464 // 28 layers × 512 kv_dim (GQA)
	}
	return governorFallbackKVPerTok
}

// governorBudgetBytes computes the memory ceiling from detected capabilities:
// free RAM (falling back to 70% of total when the detector couldn't measure
// availability) plus VRAM, scaled by the safe fraction.
func governorBudgetBytes(caps *capability.SystemCapabilities) int64 {
	if caps == nil {
		return 0
	}
	free := int64(caps.Memory.AvailableBytes)
	if free <= 0 {
		free = int64(float64(caps.Memory.TotalBytes) * 0.70)
	}
	return int64(float64(free+int64(caps.GPU.VRAMBytes)) * governorSafeFraction)
}

// PlanLocalLoad picks the largest candidate model that fits the machine, with
// the largest context window that keeps the TOTAL bill (model + KV + LoRAs +
// overhead) inside the budget. desiredCtx > 0 is the user's
// embedded_context_size wish — honored as the starting point but still
// laddered down if it doesn't fit, because no configuration value is allowed
// to launch an over-budget process (that is the hang).
//
// Ladder, per candidate (largest model first): start at the desired context,
// halve down to the floor; the first fitting (model, ctx) wins. NParallel is
// then chosen to keep the effective window usable: 2 slots (completion +
// embedding concurrently) when the window is comfortable, 1 slot when context
// had to shrink so a single request keeps the whole window.
func PlanLocalLoad(caps *capability.SystemCapabilities, candidates []ModelFile, loraBytes int64, desiredCtx int) LoadPlan {
	budget := governorBudgetBytes(caps)
	if budget <= 0 {
		return LoadPlan{Fits: false, Refusal: "hardware capabilities unknown — cannot budget a local model safely"}
	}
	if len(candidates) == 0 {
		return LoadPlan{Fits: false, Refusal: "no local model files available"}
	}

	// Largest model first: prefer capability, degrade context before quality.
	sorted := make([]ModelFile, len(candidates))
	copy(sorted, candidates)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Bytes > sorted[i].Bytes {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var minNeeded int64 // smallest total bill seen, for the refusal message
	for _, m := range sorted {
		kvPerTok := kvBytesPerCtxToken(m.Path)
		startCtx := desiredCtx
		if startCtx <= 0 {
			startCtx = 32768
		}
		if native := catalogContextWindow(filepath.Base(m.Path)); native > 0 && startCtx > native {
			startCtx = native // llama-server rejects -c beyond the trained window
		}
		for ctx := startCtx; ctx >= governorCtxFloor; ctx /= 2 {
			total := m.Bytes + kvPerTok*int64(ctx) + loraBytes + governorOverheadBytes
			if minNeeded == 0 || total < minNeeded {
				minNeeded = total
			}
			if total <= budget {
				np := 2
				if ctx < 16384 {
					np = 1 // preserve a usable effective window on tight RAM
				}
				return LoadPlan{
					ModelPath:  m.Path,
					ModelBytes: m.Bytes,
					KVBytes:    kvPerTok * int64(ctx),
					LoRABytes:  loraBytes,
					NCtx:       ctx,
					NParallel:  np,
					Fits:       true,
				}
			}
		}
	}

	return LoadPlan{
		Fits: false,
		Refusal: fmt.Sprintf(
			"local model needs ≥ %.1f GB (model+KV+LoRA+overhead) but the safe budget is %.1f GB of free memory — staying cloud-only",
			float64(minNeeded)/(1<<30), float64(budget)/(1<<30)),
	}
}

// LoRADirBytes sums the .gguf adapter files in dir — the resident cost of
// pre-loading them at launch (--lora-init-without-apply keeps them at scale 0
// but they still occupy memory). 0 for a missing/empty dir.
func LoRADirBytes(dir string) int64 {
	if dir == "" {
		dir = defaultLoRADir()
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.gguf"))
	if err != nil {
		return 0
	}
	var total int64
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			total += fi.Size()
		}
	}
	return total
}
