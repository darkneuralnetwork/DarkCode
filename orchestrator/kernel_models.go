package orchestrator

import "time"

type RouterModelInfo struct {
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Status        string    `json:"status"`
	Tier          string    `json:"tier"`
	Endpoints     int       `json:"endpoints"`
	IsPrimary     bool      `json:"is_primary"`
	Disabled      bool      `json:"disabled"`
	DisabledUntil time.Time `json:"disabled_until,omitempty"`
}

func (k *Kernel) SequentialMode() bool {
	return k.cfg.MaxConcurrent <= 1
}

// SetPlanControls hot-applies the plan gate policy ("always"/"auto"/"never")
// and planning-depth override ("auto"/"light"/"deep") from the Settings UI.
// Empty strings leave the current value unchanged.
func (k *Kernel) SetPlanControls(approval, depth string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if approval != "" {
		k.cfg.PlanApproval = approval
	}
	if depth != "" {
		k.cfg.PlanDepth = depth
	}
}

func (k *Kernel) SetExecutionProfile(profile string) {
	k.cfg.ExecutionProfile = profile
	if profile == "sequential" {
		k.cfg.MaxConcurrent = 1
	} else if profile == "parallel" {
		k.cfg.MaxConcurrent = 10
	}
}

func (k *Kernel) SetModelRole(modelName, role string) {
	k.router.SetModelRole(modelName, role)
}

// DisableModel temporarily takes a registered model out of routing/consensus
// selection until the given time (local-first upgrade §6c). Thin delegation
// to the router — see router.Router.DisableModel for the actual mechanism
// (lazy expiry, escalation-only-style routing around the disabled model).
func (k *Kernel) DisableModel(modelName string, until time.Time) {
	k.router.DisableModel(modelName, until)
}

// EnableModel reverses a temporary disable early. A no-op if the model
// wasn't disabled (lazy expiry already handles the normal "duration
// elapsed" case with no explicit call needed).
func (k *Kernel) EnableModel(modelName string) {
	k.router.EnableModel(modelName)
}

// IsModelDisabled reports whether modelName is currently temporarily
// disabled — for CLI/GUI status displays.
func (k *Kernel) IsModelDisabled(modelName string) bool {
	return k.router.IsModelDisabled(modelName)
}

func (k *Kernel) RegisteredModels() []RouterModelInfo {
	var info []RouterModelInfo
	// Pull from router
	if k.router != nil {
		now := time.Now()
		for _, m := range k.router.AllModels() {
			info = append(info, RouterModelInfo{
				Name:          m.Name,
				Role:          m.Role,
				Tier:          string(m.Tier),
				IsPrimary:     m.Role == "synthesizer" || m.Role == "primary", // Basic heuristic
				Disabled:      !m.DisabledUntil.IsZero() && now.Before(m.DisabledUntil),
				DisabledUntil: m.DisabledUntil,
			})
		}
	}
	return info
}
