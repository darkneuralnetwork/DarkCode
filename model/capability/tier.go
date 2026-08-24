package capability

// ExecutionTier determines what models and tools the system can run locally.
type ExecutionTier int

const (
	TierDeterministicOnly ExecutionTier = iota // <4GB RAM, no GPU. No local models.
	TierTinyLocal                              // 4-8GB RAM. 1-3B models.
	TierMediumLocal                            // 8-16GB RAM. 7-13B models.
	TierHybrid                                 // 16-32GB RAM. 7-70B local + cloud.
	TierCloudEnhanced                          // 32GB+ RAM or VRAM >=8GB. Full local + cloud.
)

func (t ExecutionTier) String() string {
	switch t {
	case TierDeterministicOnly:
		return "DeterministicOnly"
	case TierTinyLocal:
		return "TinyLocal"
	case TierMediumLocal:
		return "MediumLocal"
	case TierHybrid:
		return "Hybrid"
	case TierCloudEnhanced:
		return "CloudEnhanced"
	default:
		return "Unknown"
	}
}

const (
	GB = 1024 * 1024 * 1024
)

// AssignTier determines the execution tier based on system capabilities.
func AssignTier(caps *SystemCapabilities) ExecutionTier {
	ramGB := float64(caps.Memory.TotalBytes) / float64(GB)
	vramGB := float64(caps.GPU.VRAMBytes) / float64(GB)

	// If we have a powerful GPU (>= 8GB VRAM) and at least 16GB RAM
	if vramGB >= 8.0 && ramGB >= 15.0 {
		return TierCloudEnhanced
	}

	// High-end machine without a massive dedicated GPU, or decent GPU
	if ramGB >= 31.0 || (ramGB >= 15.0 && vramGB >= 4.0) {
		return TierCloudEnhanced
	}

	// Good machine (16GB RAM)
	if ramGB >= 15.0 {
		return TierHybrid
	}

	// Medium machine (8GB RAM)
	if ramGB >= 7.0 {
		return TierMediumLocal
	}

	// Low-end machine (4GB RAM)
	if ramGB >= 3.5 {
		return TierTinyLocal
	}

	// Very low end
	return TierDeterministicOnly
}
