package scheduler

import (
	"context"
	"fmt"
	"sync"
)

// ModelLoader handles loading/unloading models while respecting the memory budget.
type ModelLoader struct {
	mu           sync.Mutex
	loadedModels map[string]int64 // map of modelName -> bytes
	budget       *MemoryBudget
}

func NewModelLoader(budget *MemoryBudget) *ModelLoader {
	return &ModelLoader{
		loadedModels: make(map[string]int64),
		budget:       budget,
	}
}

// LoadModel loads a model into memory if the budget allows.
func (ml *ModelLoader) LoadModel(ctx context.Context, name string, size int64) error {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if _, ok := ml.loadedModels[name]; ok {
		return nil // Already loaded
	}

	if err := ml.budget.Allocate(size); err != nil {
		return fmt.Errorf("failed to load model %s: %w", name, err)
	}

	ml.loadedModels[name] = size
	return nil
}

// UnloadModel frees memory from a loaded model.
func (ml *ModelLoader) UnloadModel(name string) {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if size, ok := ml.loadedModels[name]; ok {
		ml.budget.Free(size)
		delete(ml.loadedModels, name)
	}
}
