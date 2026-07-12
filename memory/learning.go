package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// ============================================================================
// LEARNING ENGINE
//
// Processes task outcomes to extract reusable strategies and optimize
// future execution. Implements the feedback loop:
//   Execute → Evaluate → Extract Patterns → Store → Reuse
// ============================================================================

// LearningEngine tracks task performance and extracts strategies.
type LearningEngine struct {
	mu          sync.RWMutex
	feedback    []core.LearningFeedback
	strategies  map[string]*core.LearnedStrategy
	filePath    string
	stratPath   string
	fbWriter    *DebouncedWriter
	stratWriter *DebouncedWriter
}

// learningData is the serialized form.
type learningData struct {
	Feedback   []core.LearningFeedback          `json:"feedback"`
	Strategies map[string]*core.LearnedStrategy `json:"strategies"`
}

// NewLearningEngine creates a learning engine with persistence.
func NewLearningEngine(dir string) (*LearningEngine, error) {
	le := &LearningEngine{
		strategies: make(map[string]*core.LearnedStrategy),
		filePath:   filepath.Join(dir, "learning_feedback.json"),
		stratPath:  filepath.Join(dir, "learned_strategies.json"),
	}
	if err := le.load(); err != nil {
		return nil, err
	}

	le.fbWriter = NewDebouncedWriter(le.filePath, 2*time.Second, func() ([]byte, error) {
		le.mu.RLock()
		defer le.mu.RUnlock()
		return json.Marshal(le.feedback)
	})
	le.stratWriter = NewDebouncedWriter(le.stratPath, 2*time.Second, func() ([]byte, error) {
		le.mu.RLock()
		defer le.mu.RUnlock()
		return json.Marshal(le.strategies)
	})

	return le, nil
}

// Shutdown flushes pending writes to disk.
func (le *LearningEngine) Shutdown() {
	if le.fbWriter != nil {
		le.fbWriter.Shutdown()
	}
	if le.stratWriter != nil {
		le.stratWriter.Shutdown()
	}
}

// RecordFeedback stores a task outcome for learning.
func (le *LearningEngine) RecordFeedback(fb core.LearningFeedback) error {
	le.mu.Lock()
	defer le.mu.Unlock()

	if fb.Timestamp.IsZero() {
		fb.Timestamp = time.Now()
	}
	if fb.TaskType == "" {
		fb.TaskType = classifyTaskType(fb.TaskGoal)
	}

	le.feedback = append(le.feedback, fb)

	// Auto-extract strategy if we have enough data
	le.maybeExtractStrategy(fb)

	le.fbWriter.MarkDirty()
	le.stratWriter.MarkDirty()
	return nil
}

// GetStrategy returns the best strategy for a task type, if one exists.
func (le *LearningEngine) GetStrategy(taskType string) (*core.LearnedStrategy, bool) {
	le.mu.RLock()
	defer le.mu.RUnlock()
	s, ok := le.strategies[taskType]
	if !ok {
		return nil, false
	}
	// Only return strategies that have proven successful
	successRate := float64(s.SuccessCount) / float64(s.SuccessCount+s.FailCount)
	if successRate < 0.5 {
		return nil, false
	}
	return s, true
}

// SuggestStrategy queries all strategies for the best match to a goal.
func (le *LearningEngine) SuggestStrategy(goal string) *core.LearnedStrategy {
	le.mu.RLock()
	defer le.mu.RUnlock()

	taskType := classifyTaskType(goal)
	if s, ok := le.strategies[taskType]; ok {
		successRate := float64(s.SuccessCount) / float64(s.SuccessCount+s.FailCount)
		if successRate >= 0.5 {
			return s
		}
	}
	return nil
}

// GetAllStrategies returns all learned strategies.
func (le *LearningEngine) GetAllStrategies() []*core.LearnedStrategy {
	le.mu.RLock()
	defer le.mu.RUnlock()
	result := make([]*core.LearnedStrategy, 0, len(le.strategies))
	for _, s := range le.strategies {
		result = append(result, s)
	}
	return result
}

// GetRecentFeedback returns the N most recent feedback entries.
func (le *LearningEngine) GetRecentFeedback(n int) []core.LearningFeedback {
	le.mu.RLock()
	defer le.mu.RUnlock()

	if n > len(le.feedback) {
		n = len(le.feedback)
	}
	result := make([]core.LearningFeedback, n)
	copy(result, le.feedback[len(le.feedback)-n:])
	// Reverse for most-recent-first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// GetStats returns aggregate performance statistics.
func (le *LearningEngine) GetStats() map[string]interface{} {
	le.mu.RLock()
	defer le.mu.RUnlock()

	total := len(le.feedback)
	successes := 0
	toolUsage := make(map[string]int)
	agentUsage := make(map[string]int)
	taskTypes := make(map[string]int)

	for _, fb := range le.feedback {
		if fb.Success {
			successes++
		}
		for _, t := range fb.ToolsUsed {
			toolUsage[t]++
		}
		for _, a := range fb.AgentsUsed {
			agentUsage[string(a)]++
		}
		taskTypes[fb.TaskType]++
	}

	var successRate float64
	if total > 0 {
		successRate = float64(successes) / float64(total)
	}

	return map[string]interface{}{
		"total_tasks":    total,
		"success_rate":   math.Round(successRate*100) / 100,
		"strategy_count": len(le.strategies),
		"tool_usage":     toolUsage,
		"agent_usage":    agentUsage,
		"task_types":     taskTypes,
	}
}

// maybeExtractStrategy checks if we have enough data to extract a strategy.
// Called under write lock.
func (le *LearningEngine) maybeExtractStrategy(latest core.LearningFeedback) {
	taskType := latest.TaskType
	if taskType == "" {
		return
	}

	// Count successes and failures for this task type
	var successes, failures int
	toolCounts := make(map[string]int)
	agentCounts := make(map[string]int)

	for _, fb := range le.feedback {
		if fb.TaskType != taskType {
			continue
		}
		if fb.Success {
			successes++
			for _, t := range fb.ToolsUsed {
				toolCounts[t]++
			}
			for _, a := range fb.AgentsUsed {
				agentCounts[string(a)]++
			}
		} else {
			failures++
		}
	}

	// Need at least 2 successes to extract a strategy
	if successes < 2 {
		return
	}

	// Find most-used tools and agents in successful runs
	var preferredTools []string
	for tool, count := range toolCounts {
		if count >= successes/2 {
			preferredTools = append(preferredTools, tool)
		}
	}

	var preferredAgents []core.AgentRole
	for agent, count := range agentCounts {
		if count >= successes/2 {
			preferredAgents = append(preferredAgents, core.AgentRole(agent))
		}
	}

	existing, exists := le.strategies[taskType]
	if exists {
		existing.SuccessCount = successes
		existing.FailCount = failures
		existing.PreferredTools = preferredTools
		existing.PreferredAgents = preferredAgents
		now := time.Now()
		existing.LastUsed = &now
	} else {
		le.strategies[taskType] = &core.LearnedStrategy{
			Name:            fmt.Sprintf("strategy_%s", taskType),
			Description:     fmt.Sprintf("Auto-learned strategy for %s tasks", taskType),
			TaskType:        taskType,
			PreferredTools:  preferredTools,
			PreferredAgents: preferredAgents,
			SuccessCount:    successes,
			FailCount:       failures,
			CreatedAt:       time.Now(),
		}
	}
}

// classifyTaskType classifies a task goal into a task type.
func classifyTaskType(goal string) string {
	goalLower := strings.ToLower(goal)

	classifiers := []struct {
		keywords []string
		taskType string
	}{
		{[]string{"debug", "fix", "error", "bug", "issue"}, "debug"},
		{[]string{"test", "check", "verify", "validate"}, "testing"},
		{[]string{"deploy", "release", "publish", "ship"}, "deployment"},
		{[]string{"refactor", "clean", "optimize", "improve"}, "refactor"},
		{[]string{"build", "create", "implement", "write", "add", "code"}, "code"},
		{[]string{"search", "find", "look", "research", "analyze"}, "research"},
		{[]string{"design", "architect", "plan", "structure"}, "design"},
		{[]string{"document", "readme", "comment", "explain"}, "documentation"},
		{[]string{"config", "setup", "install", "configure"}, "configuration"},
		{[]string{"security", "vulnerability", "audit", "scan"}, "security"},
	}

	for _, c := range classifiers {
		for _, kw := range c.keywords {
			if strings.Contains(goalLower, kw) {
				return c.taskType
			}
		}
	}
	return "general"
}

// ============================================================================
// PERSISTENCE
// ============================================================================

func (le *LearningEngine) load() error {
	// Load feedback
	if data, err := os.ReadFile(le.filePath); err == nil {
		if err := json.Unmarshal(data, &le.feedback); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// Load strategies
	if data, err := os.ReadFile(le.stratPath); err == nil {
		if err := json.Unmarshal(data, &le.strategies); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	return nil
}
