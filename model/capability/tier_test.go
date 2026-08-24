package capability

import "testing"

func capsWith(ramGB, vramGB float64) *SystemCapabilities {
	return &SystemCapabilities{
		Memory: MemoryInfo{TotalBytes: uint64(ramGB * float64(GB))},
		GPU:    GPUInfo{VRAMBytes: uint64(vramGB * float64(GB))},
	}
}

func TestAssignTier(t *testing.T) {
	cases := []struct {
		name  string
		ramGB float64
		vram  float64
		want  ExecutionTier
	}{
		{"tiny 2GB", 2, 0, TierDeterministicOnly},
		{"low 4GB", 4, 0, TierTinyLocal},
		{"medium 8GB", 8, 0, TierMediumLocal},
		{"good 16GB", 16, 0, TierHybrid},
		{"high 32GB", 32, 0, TierCloudEnhanced},
		{"gpu box 16GB+8GB VRAM", 16, 8, TierCloudEnhanced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssignTier(capsWith(tc.ramGB, tc.vram)); got != tc.want {
				t.Errorf("AssignTier(ram=%.0f vram=%.0f) = %v, want %v", tc.ramGB, tc.vram, got, tc.want)
			}
		})
	}
}

func TestAdvisorGating(t *testing.T) {
	// A deterministic-only machine must refuse local models; a 16GB one allows them.
	low := NewAdvisor(capsWith(2, 0))
	if low.CanRunLocalModels() {
		t.Error("deterministic-only tier must not run local models")
	}
	high := NewAdvisor(capsWith(16, 0))
	if !high.CanRunLocalModels() {
		t.Error("hybrid tier (16GB) should run local models")
	}
	if high.RecommendedConcurrency() < 1 {
		t.Error("recommended concurrency must be at least 1")
	}
}
