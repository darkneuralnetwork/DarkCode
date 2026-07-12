package capability

// advisor.go — translates an ExecutionTier into actionable recommendations
// for the router, scheduler, and model manager. This is the "wiring" that was
// missing (spec audit: "tier only consumed in 2 display paths, does NOT drive
// scheduler/router/model selection").
//
// The Advisor answers questions like:
//   - Can this system run a local model at all?
//   - What's the largest local model that fits in RAM/VRAM?
//   - Should the router prefer local or cloud for a given task?
//   - How many concurrent tasks can the scheduler safely run?

// Advisor translates hardware capabilities into execution recommendations.
type Advisor struct {
	caps *SystemCapabilities
	tier ExecutionTier
}

// NewAdvisor creates an advisor from detected capabilities.
func NewAdvisor(caps *SystemCapabilities) *Advisor {
	return &Advisor{
		caps: caps,
		tier: AssignTier(caps),
	}
}

// Tier returns the execution tier.
func (a *Advisor) Tier() ExecutionTier { return a.tier }

// CanRunLocalModels reports whether the system has enough resources to load
// any local model (≥4 GB RAM).
func (a *Advisor) CanRunLocalModels() bool {
	return a.tier >= TierTinyLocal
}

// MaxLocalModelBytes returns the recommended maximum model size (in bytes)
// that can be loaded without exhausting system memory. Reserves 50% of the
// model budget for the OS + context processing.
func (a *Advisor) MaxLocalModelBytes() int64 {
	ram := int64(a.caps.Memory.TotalBytes)
	vram := int64(a.caps.GPU.VRAMBytes)

	// If we have a GPU, model goes in VRAM (use 80% of VRAM).
	if vram > 0 {
		return int64(float64(vram) * 0.8)
	}
	// CPU-only: use 30% of RAM (models need headroom for inference + OS).
	return int64(float64(ram) * 0.3)
}

// PreferredModelSize returns the recommended parameter count (in billions)
// for local models on this system.
func (a *Advisor) PreferredModelSize() string {
	switch a.tier {
	case TierDeterministicOnly:
		return "none" // no local models; cloud-only
	case TierTinyLocal:
		return "1-3B"
	case TierMediumLocal:
		return "7-13B"
	case TierHybrid:
		return "7-13B local + cloud fallback"
	case TierCloudEnhanced:
		return "7-70B local + cloud"
	}
	return "unknown"
}

// PreferLocal reports whether the router should prefer a local model over a
// cloud API for latency-sensitive tasks. True when the system can run a
// medium+ model locally (lower latency, no network, privacy).
func (a *Advisor) PreferLocal() bool {
	return a.tier >= TierMediumLocal
}

// RecommendedConcurrency returns how many concurrent tasks the scheduler
// should run. Scales with CPU cores but caps based on memory.
func (a *Advisor) RecommendedConcurrency() int {
	cores := a.caps.CPU.Cores
	if cores < 1 {
		cores = 1
	}
	ramGB := float64(a.caps.Memory.TotalBytes) / float64(GB)
	// Cap by memory: each concurrent task needs ~1GB headroom.
	memCap := int(ramGB / 2)
	if memCap < 1 {
		memCap = 1
	}
	if cores < memCap {
		return cores
	}
	return memCap
}

// RecommendedContextWindow returns the max context window (in tokens) the
// system can comfortably handle. Larger RAM → larger context.
func (a *Advisor) RecommendedContextWindow() int {
	ramGB := float64(a.caps.Memory.TotalBytes) / float64(GB)
	switch {
	case ramGB >= 31:
		return 128000
	case ramGB >= 15:
		return 32000
	case ramGB >= 7:
		return 8192
	default:
		return 4096
	}
}

// FallbackToCloud reports whether cloud APIs should be the primary path
// (true on low-end systems that can't run local models).
func (a *Advisor) FallbackToCloud() bool {
	return a.tier <= TierTinyLocal
}

// Summary returns a human-readable summary for the dashboard/UI.
func (a *Advisor) Summary() map[string]interface{} {
	return map[string]interface{}{
		"tier":                  a.tier.String(),
		"can_run_local":         a.CanRunLocalModels(),
		"max_local_model_gb":    float64(a.MaxLocalModelBytes()) / float64(GB),
		"preferred_model_size":  a.PreferredModelSize(),
		"prefer_local":          a.PreferLocal(),
		"recommended_concurrency": a.RecommendedConcurrency(),
		"recommended_context":   a.RecommendedContextWindow(),
		"fallback_to_cloud":     a.FallbackToCloud(),
	}
}
