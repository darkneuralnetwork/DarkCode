package orchestrator

import (
	"github.com/darkcode/core"
)

// PlanningController handles task decomposition and DAG creation.
type PlanningController struct {
	router core.ModelRouter
}

func NewPlanningController(router core.ModelRouter) *PlanningController {
	return &PlanningController{router: router}
}

// ExecutionController coordinates DAG execution across agents.
type ExecutionController struct {
	kernel *Kernel
}

func NewExecutionController(k *Kernel) *ExecutionController {
	return &ExecutionController{kernel: k}
}

// ContextController manages context assembly and triggering compression.
type ContextController struct {
	// engine *ctxengine.Engine
}

func NewContextController() *ContextController {
	return &ContextController{}
}

// MemoryController manages storage to episodic/semantic layers.
type MemoryController struct {
	mem core.MemoryStore
}

func NewMemoryController(m core.MemoryStore) *MemoryController {
	return &MemoryController{mem: m}
}

// VerifyController handles the deterministic verification pipeline.
type VerifyController struct {
	// pipeline *agents.VerificationPipeline
}

func NewVerifyController() *VerifyController {
	return &VerifyController{}
}
