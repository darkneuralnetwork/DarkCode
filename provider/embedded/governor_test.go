package embedded

import (
	"strings"
	"testing"

	"github.com/darkcode/capability"
)

func capsWithRAM(totalGB float64, vramGB float64) *capability.SystemCapabilities {
	c := &capability.SystemCapabilities{}
	c.Memory.TotalBytes = uint64(totalGB * float64(1<<30))
	c.GPU.VRAMBytes = uint64(vramGB * float64(1<<30))
	return c
}

var (
	tinyModel   = ModelFile{Path: "qwen1_5-0_5b-chat-q4_k_m.gguf", Bytes: 398 << 20}
	mediumModel = ModelFile{Path: "qwen2_5-1_5b-instruct-q4_k_m.gguf", Bytes: 1000 << 20}
	bigModel    = ModelFile{Path: "qwen2_5-7b-instruct-q4_k_m.gguf", Bytes: 4700 << 20}
)

// An 11GB machine (the reported hang profile) must get a plan whose total
// bill fits the safe budget — never an over-budget launch.
func TestPlanLocalLoad_11GBNeverPlansOverBudget(t *testing.T) {
	caps := capsWithRAM(11, 0)
	plan := PlanLocalLoad(caps, []ModelFile{mediumModel, tinyModel}, 0, 0)
	if !plan.Fits {
		t.Fatalf("11GB machine must fit the 1.5B model, got refusal: %s", plan.Refusal)
	}
	budget := governorBudgetBytes(caps)
	total := plan.ModelBytes + plan.KVBytes + plan.LoRABytes + governorOverheadBytes
	if total > budget {
		t.Errorf("planned total %d exceeds budget %d — this is the swap-thrash hang", total, budget)
	}
	if plan.ModelPath != mediumModel.Path {
		t.Errorf("expected the larger fitting model %s, got %s", mediumModel.Path, plan.ModelPath)
	}
	if plan.NCtx <= 0 || plan.NParallel <= 0 {
		t.Errorf("plan must set launch values, got nCtx=%d np=%d", plan.NCtx, plan.NParallel)
	}
}

// A 7B model that cannot fit must fall through to a smaller candidate rather
// than being force-planned.
func TestPlanLocalLoad_FallsThroughToSmallerModel(t *testing.T) {
	plan := PlanLocalLoad(capsWithRAM(11, 0), []ModelFile{bigModel, mediumModel}, 0, 0)
	if !plan.Fits {
		t.Fatalf("expected a fit via the smaller candidate, got refusal: %s", plan.Refusal)
	}
	if plan.ModelPath != mediumModel.Path {
		t.Errorf("expected fallthrough to %s, got %s", mediumModel.Path, plan.ModelPath)
	}
}

// On a tight machine the ladder shrinks the context window (the KV cache is
// the dominant cost for the MHA 0.5B) instead of refusing outright, and
// drops to a single parallel slot to preserve the effective window.
func TestPlanLocalLoad_LaddersContextDownOnTightRAM(t *testing.T) {
	plan := PlanLocalLoad(capsWithRAM(3, 0), []ModelFile{tinyModel}, 0, 0)
	if !plan.Fits {
		t.Fatalf("3GB machine should fit the 0.5B at a reduced context, got refusal: %s", plan.Refusal)
	}
	if plan.NCtx >= 32768 {
		t.Errorf("expected a laddered-down context on 3GB, got nCtx=%d", plan.NCtx)
	}
	if plan.NParallel != 1 {
		t.Errorf("small context must use 1 slot to preserve the effective window, got np=%d", plan.NParallel)
	}
}

// When nothing fits, the governor refuses with a human-readable reason —
// never a silent nil or an over-budget plan.
func TestPlanLocalLoad_RefusesWithReasonWhenNothingFits(t *testing.T) {
	plan := PlanLocalLoad(capsWithRAM(1, 0), []ModelFile{tinyModel, mediumModel}, 0, 0)
	if plan.Fits {
		t.Fatalf("1GB machine must refuse, got plan %+v", plan)
	}
	if !strings.Contains(plan.Refusal, "GB") {
		t.Errorf("refusal must state the numbers, got: %q", plan.Refusal)
	}
}

// A user context override is the ladder's starting point but is still
// laddered down when over budget: no configuration value may launch an
// over-budget process.
func TestPlanLocalLoad_UserContextOverrideCannotBlowBudget(t *testing.T) {
	caps := capsWithRAM(6, 0)
	plan := PlanLocalLoad(caps, []ModelFile{mediumModel}, 0, 131072)
	if !plan.Fits {
		t.Fatalf("expected a laddered fit, got refusal: %s", plan.Refusal)
	}
	// Native window for the 1.5B is 32768, so the wish is clamped there
	// first, then laddered if needed.
	if plan.NCtx > 32768 {
		t.Errorf("nCtx %d exceeds the model's native window", plan.NCtx)
	}
	budget := governorBudgetBytes(caps)
	if total := plan.ModelBytes + plan.KVBytes + governorOverheadBytes; total > budget {
		t.Errorf("override produced an over-budget plan: %d > %d", total, budget)
	}
}

// LoRA bytes are part of the bill: a plan that fits without adapters must
// shrink (or refuse) when several GB of adapters will be resident.
func TestPlanLocalLoad_LoRABytesCountAgainstBudget(t *testing.T) {
	caps := capsWithRAM(6, 0)
	without := PlanLocalLoad(caps, []ModelFile{mediumModel}, 0, 0)
	with := PlanLocalLoad(caps, []ModelFile{mediumModel}, 3<<30, 0)
	if !without.Fits {
		t.Fatalf("baseline should fit: %s", without.Refusal)
	}
	if with.Fits && with.NCtx >= without.NCtx {
		t.Errorf("3GB of LoRAs must shrink the plan (or refuse): without=%d with=%d", without.NCtx, with.NCtx)
	}
}

func TestLoadPlanEffectiveWindow(t *testing.T) {
	p := LoadPlan{NCtx: 32768, NParallel: 2}
	if got := p.EffectiveWindow(); got != 16384 {
		t.Errorf("EffectiveWindow = %d, want 16384", got)
	}
	p = LoadPlan{NCtx: 8192, NParallel: 0}
	if got := p.EffectiveWindow(); got != 8192 {
		t.Errorf("EffectiveWindow with np=0 = %d, want 8192 (no split)", got)
	}
}

// Free memory, not total, is the budget base: the same total RAM with little
// available memory must produce a smaller (or refused) plan.
func TestGovernorBudgetUsesAvailableMemory(t *testing.T) {
	tight := capsWithRAM(11, 0)
	tight.Memory.AvailableBytes = 2 << 30 // 11GB machine with only 2GB free
	roomy := capsWithRAM(11, 0)           // no measurement → 70% fallback
	if governorBudgetBytes(tight) >= governorBudgetBytes(roomy) {
		t.Errorf("2GB-free budget must be below the 70%%-of-total fallback")
	}
}
