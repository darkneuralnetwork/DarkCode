package core

import (
	"strings"
	"time"
)

// ============================================================================
// LAYER 1 — ORCHESTRATION TYPES: DAG, Tasks, Execution Loop
// ============================================================================

// TaskStatus represents the lifecycle state of a task node.
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskPlanning   TaskStatus = "planning"
	TaskReady      TaskStatus = "ready"
	TaskRunning    TaskStatus = "running"
	TaskValidating TaskStatus = "validating"
	TaskCompleted  TaskStatus = "completed"
	TaskFailed     TaskStatus = "failed"
	TaskCancelled  TaskStatus = "cancelled"
	TaskBlocked    TaskStatus = "blocked" // waiting on dependency
)

// TaskPriority controls execution order when multiple tasks are ready.
type TaskPriority string

const (
	PriorityCritical TaskPriority = "critical"
	PriorityHigh     TaskPriority = "high"
	PriorityNormal   TaskPriority = "normal"
	PriorityLow      TaskPriority = "low"
)

// TaskNode represents a single node in the task DAG.
type TaskNode struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Goal         string            `json:"goal"`
	Description  string            `json:"description,omitempty"`
	Status       TaskStatus        `json:"status"`
	Priority     TaskPriority      `json:"priority"`
	Dependencies []string          `json:"dependencies,omitempty"` // task IDs this depends on
	AgentRole    AgentRole         `json:"agent_role"`             // which sub-agent type should handle this
	ModelTier    ModelTier         `json:"model_tier"`             // which model tier to use
	Input        string            `json:"input,omitempty"`        // input context for this task
	Output       string            `json:"output,omitempty"`       // result of this task
	Error        string            `json:"error,omitempty"`
	ToolCalls    []ToolCall        `json:"tool_calls,omitempty"` // tools used by this task
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// CanExecute returns true if all dependencies are completed.
func (t *TaskNode) CanExecute(completed map[string]bool) bool {
	for _, dep := range t.Dependencies {
		if !completed[dep] {
			return false
		}
	}
	return true
}

// Duration returns the execution time, or zero if not finished.
func (t *TaskNode) Duration() time.Duration {
	if t.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if t.CompletedAt != nil {
		end = *t.CompletedAt
	}
	return end.Sub(*t.StartedAt)
}

// ============================================================================
// LAYER 2 — MODEL ROUTING TYPES
// ============================================================================

// ModelTier identifies which class of model should handle a task.
type ModelTier string

const (
	ModelTierReasoning   ModelTier = "reasoning" // strongest model for planning
	ModelTierCoding      ModelTier = "coding"    // mid-high for code execution
	ModelTierFast        ModelTier = "fast"      // lightweight for simple tasks
	ModelTierLocal       ModelTier = "local"     // generic open-source fallback
	ModelTierCritic      ModelTier = "critic"    // independent verifier
	ModelTierMediumLocal ModelTier = "medium_local" // medium local model (e.g. review code)
	ModelTierTinyLocal   ModelTier = "tiny_local"   // tiny local model (e.g. explain error)
)

// RoutingMode controls how models are selected.
type RoutingMode string

const (
	RouteSingle     RoutingMode = "single"     // one best model
	RouteEscalation RoutingMode = "escalation" // escalate on uncertainty
	RouteConsensus  RoutingMode = "consensus"  // parallel multi-model
)

// ModelConfig defines a single model endpoint.
type ModelConfig struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ModelContribution holds a single non-primary model's output in a consensus
// round. Each model answers the query with its assigned role persona (critic,
// skeptic, knowledge_booster, etc.). The primary model then synthesizes all
// contributions into a single consensus answer.
type ModelContribution struct {
	Model  string `json:"model"`           // model name
	Role   string `json:"role"`            // consensus role
	Output string `json:"output"`          // the model's answer
	Error  string `json:"error,omitempty"` // non-empty if the call failed
}

// ConsensusResult holds the output of a multi-model consensus round.
type ConsensusResult struct {
	Primary       string              `json:"primary"`                 // primary model's direct answer
	Critique      string              `json:"critique"`                // (legacy) critic output
	Verify        string              `json:"verify"`                  // (legacy) verification output
	Synthesized   string              `json:"synthesized"`             // merged consensus answer
	Conflict      bool                `json:"conflict"`                // unresolved contradiction
	Contributions []ModelContribution `json:"contributions,omitempty"` // per-model outputs
}

// ============================================================================
// LAYER 3 — COMPRESSION TYPES
// ============================================================================

// ContextSnapshot is the compressed representation of conversation state.
type ContextSnapshot struct {
	Goal             string    `json:"goal"`
	ActiveTasks      []string  `json:"active_tasks"`
	Constraints      []string  `json:"constraints"`
	Decisions        []string  `json:"decisions"`
	Errors           []string  `json:"errors"`
	ImportantContext []string  `json:"important_context"`
	NextActions      []string  `json:"next_actions"`
	CompressedAt     time.Time `json:"compressed_at"`
	OriginalTokens   int       `json:"original_tokens"` // estimated
	CompressedTokens int       `json:"compressed_tokens"`
}

// CompressionRatio returns how much the context was reduced.
func (c *ContextSnapshot) CompressionRatio() float64 {
	if c.OriginalTokens == 0 {
		return 0
	}
	return 1.0 - float64(c.CompressedTokens)/float64(c.OriginalTokens)
}

// ============================================================================
// LAYER 4 — MEMORY TYPES
// ============================================================================

// MemoryType identifies which of the 4 memory systems an entry belongs to.
type MemoryType string

const (
	MemShortTerm  MemoryType = "short_term" // active conversation window
	MemEpisodic   MemoryType = "episodic"   // past tasks and outcomes
	MemSemantic   MemoryType = "semantic"   // knowledge, docs, code
	MemProcedural MemoryType = "procedural" // skills/workflows
)

// Skill represents a reusable procedural memory entry.
type Skill struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Steps       []SkillStep       `json:"steps"`
	TriggerCond string            `json:"trigger_condition,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	LastUsed    *time.Time        `json:"last_used,omitempty"`
	UseCount    int               `json:"use_count"`
	SuccessRate float64           `json:"success_rate"` // 0.0-1.0
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// SkillStep is a single step in a procedural skill.
type SkillStep struct {
	Order    int    `json:"order"`
	Action   string `json:"action"`
	Tool     string `json:"tool,omitempty"`
	Expected string `json:"expected,omitempty"`
}

// EpisodicEntry records a past task execution.
type EpisodicEntry struct {
	ID             string    `json:"id"`
	TaskGoal       string    `json:"task_goal"`
	Outcome        string    `json:"outcome"` // "success" or "failure"
	Summary        string    `json:"summary"`
	Output         string    `json:"output,omitempty"` // full LLM output (for exact-match cache)
	Steps          []string  `json:"steps"`
	Duration       string    `json:"duration"`
	ModelUsed      string    `json:"model_used"`
	ToolsUsed      []string  `json:"tools_used"`
	LessonsLearned []string  `json:"lessons_learned,omitempty"`
	Vector         []float32 `json:"vector,omitempty"`
	InjectedRecall string    `json:"injected_recall,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}

// ============================================================================
// LAYER 5 — SUB-AGENT TYPES
// ============================================================================

// AgentRole identifies the type of sub-agent.
type AgentRole string

const (
	RoleExecutive   AgentRole = "executive"   // high-level control, goal tracking
	RolePlanner     AgentRole = "planner"     // task decomposition, DAG creation
	RoleWorker      AgentRole = "worker"      // coding, implementation, API calls
	RoleCritic      AgentRole = "critic"      // validation, bug detection
	RoleCompression AgentRole = "compression" // context reduction
	RoleUI          AgentRole = "ui"          // renders UI state
	RoleResearch    AgentRole = "research"    // information gathering, documentation analysis
	RoleQA          AgentRole = "qa"          // testing, quality assurance, edge-case analysis
	RoleSecurity    AgentRole = "security"    // risk analysis, vulnerability scanning
	RoleOps         AgentRole = "ops"         // deployment, monitoring, health checks
)

// SubAgentConfig configures a spawned sub-agent.
type SubAgentConfig struct {
	Role      AgentRole `json:"role"`
	Goal      string    `json:"goal"`
	ModelTier ModelTier `json:"model_tier"`
	MaxTurns  int       `json:"max_turns"`
	Tools     []string  `json:"tools,omitempty"` // tool names this agent can use
	Context   string    `json:"context,omitempty"`
	ParentID  string    `json:"parent_id,omitempty"`
}

// SubAgentResult is what a sub-agent returns.
type SubAgentResult struct {
	AgentID   string     `json:"agent_id"`
	Role      AgentRole  `json:"role"`
	Goal      string     `json:"goal"`
	Output    string     `json:"output"`
	Success   bool       `json:"success"`
	Error     string     `json:"error,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Duration  string     `json:"duration"`
}

// ============================================================================
// LAYER 6 — UI EVENT TYPES
// ============================================================================

// EventType identifies the kind of UI event.
type EventType string

const (
	EventTaskUpdate    EventType = "task_update"
	EventAgentSpawn    EventType = "agent_spawn"
	EventAgentComplete EventType = "agent_complete"
	EventToolExecution EventType = "tool_execution"
	EventModelRoute    EventType = "model_route"
	EventCompression   EventType = "compression"
	EventMemoryStore   EventType = "memory_store"
	EventFinalOutput   EventType = "final_output"
	EventError         EventType = "error"
	EventDAGUpdate     EventType = "dag_update"
	EventSkillExtract  EventType = "skill_extract"
	EventConsensus     EventType = "consensus"
	EventTokenUsage    EventType = "token_usage"
	EventFileChange    EventType = "file_change"   // a file was created/modified/deleted
	EventApproval      EventType = "approval"      // a permission decision was made
	EventChatQuery     EventType = "chat_query"    // user input from chat
	EventChatResponse  EventType = "chat_response" // agent response
	EventSyncGUI       EventType = "sync_gui"      // trigger gui reload when waking up
	EventPlanUpdated   EventType = "plan_updated"
	EventWorkflowUpdated EventType = "workflow_updated"
)

// UIEvent is a structured event emitted during execution.
type UIEvent struct {
	Type      EventType   `json:"type"`
	Status    string      `json:"status,omitempty"`
	Agent     string      `json:"agent,omitempty"`
	Goal      string      `json:"goal,omitempty"`
	Tool      string      `json:"tool,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
	TaskID    string      `json:"task_id,omitempty"`
}

// TokenUsageStats is the payload of a token_usage event, pushed live to the
// monitoring dashboard after every LLM call.
type TokenUsageStats struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	LatencyMs        int64   `json:"latency_ms"`
	Stream           bool    `json:"stream"`
	// Cumulative totals since the tracker started
	CumulativeTokens int     `json:"cumulative_tokens"`
	CumulativeCost   float64 `json:"cumulative_cost"`
	CumulativeReqs   int     `json:"cumulative_requests"`
}

// ============================================================================
// AGENT COMMUNICATION TYPES
// ============================================================================

// MessagePriority controls urgency of inter-agent messages.
type MessagePriority string

const (
	MsgPriorityCritical MessagePriority = "critical"
	MsgPriorityHigh     MessagePriority = "high"
	MsgPriorityNormal   MessagePriority = "normal"
	MsgPriorityLow      MessagePriority = "low"
)

// MessageKind identifies the type of inter-agent message.
type MessageKind string

const (
	MsgTaskAssignment  MessageKind = "task_assignment"
	MsgStatusUpdate    MessageKind = "status_update"
	MsgResultReport    MessageKind = "result_report"
	MsgCritiqueRequest MessageKind = "critique_request"
	MsgApprovalRequest MessageKind = "approval_request"
	MsgKnowledgeShare  MessageKind = "knowledge_share"
)

// AgentMessage is a structured message between agents.
type AgentMessage struct {
	ID            string          `json:"id"`
	Kind          MessageKind     `json:"kind"`
	Sender        AgentRole       `json:"sender"`
	Receiver      AgentRole       `json:"receiver"`
	Priority      MessagePriority `json:"priority"`
	Task          string          `json:"task"`
	Payload       string          `json:"payload"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Timestamp     time.Time       `json:"timestamp"`
}

// ============================================================================
// AUDIT & GOVERNANCE TYPES
// ============================================================================

// RiskLevel classifies the danger of an action.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"      // automatic execution
	RiskMedium   RiskLevel = "medium"   // logged
	RiskHigh     RiskLevel = "high"     // requires human approval
	RiskCritical RiskLevel = "critical" // blocked by default
)

// AuditEntry records a single auditable action.
type AuditEntry struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Agent      AgentRole `json:"agent"`
	Action     string    `json:"action"`
	Tool       string    `json:"tool,omitempty"`
	RiskLevel  RiskLevel `json:"risk_level"`
	Approved   bool      `json:"approved"`
	ApprovedBy string    `json:"approved_by,omitempty"` // "auto", "human", "policy"
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
}

// ============================================================================
// LEARNING ENGINE TYPES
// ============================================================================

// LearningFeedback captures the outcome of a task for the learning engine.
type LearningFeedback struct {
	TaskGoal   string             `json:"task_goal"`
	TaskType   string             `json:"task_type"` // e.g. "code", "research", "debug"
	Success    bool               `json:"success"`
	Duration   string             `json:"duration"`
	ToolsUsed  []string           `json:"tools_used"`
	AgentsUsed []AgentRole        `json:"agents_used"`
	ModelUsed  string             `json:"model_used"`
	Strategy   string             `json:"strategy"` // "direct", "dag", "consensus"
	Lessons    []string           `json:"lessons,omitempty"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Timestamp  time.Time          `json:"timestamp"`
}

// LearnedStrategy is a strategy pattern extracted from successful tasks.
type LearnedStrategy struct {
	Name            string      `json:"name"`
	Description     string      `json:"description"`
	TaskType        string      `json:"task_type"`
	PreferredTools  []string    `json:"preferred_tools"`
	PreferredAgents []AgentRole `json:"preferred_agents"`
	PreferredModel  string      `json:"preferred_model,omitempty"`
	SuccessCount    int         `json:"success_count"`
	FailCount       int         `json:"fail_count"`
	AvgDuration     string      `json:"avg_duration"`
	CreatedAt       time.Time   `json:"created_at"`
	LastUsed        *time.Time  `json:"last_used,omitempty"`
}

// ============================================================================
// SELF-VERIFICATION TYPES
// ============================================================================

// ConfidenceScore quantifies how confident the system is in a result.
type ConfidenceScore struct {
	Overall      float64 `json:"overall"`      // 0.0 to 1.0
	Evidence     float64 `json:"evidence"`     // quality of supporting evidence
	Verification float64 `json:"verification"` // consistency checks passed
	Risk         float64 `json:"risk"`         // detected risk factors
	Uncertainty  float64 `json:"uncertainty"`  // unknown/ambiguous aspects
}

// Compute returns the overall confidence using the formula:
// Confidence = (Evidence + Verification) / (1 + Risk + Uncertainty)
func (c *ConfidenceScore) Compute() float64 {
	numerator := c.Evidence + c.Verification
	denominator := 1.0 + c.Risk + c.Uncertainty
	if denominator == 0 {
		return 0
	}
	c.Overall = numerator / denominator
	if c.Overall > 1.0 {
		c.Overall = 1.0
	}
	return c.Overall
}

// VerificationResult is the output of the self-verification pipeline.
type VerificationResult struct {
	Passed       bool            `json:"passed"`
	Confidence   ConfidenceScore `json:"confidence"`
	Issues       []string        `json:"issues,omitempty"`
	Alternatives []string        `json:"alternatives,omitempty"`
	Risks        []string        `json:"risks,omitempty"`
	VerifiedAt   time.Time       `json:"verified_at"`
}

// ============================================================================
// KNOWLEDGE GRAPH TYPES
// ============================================================================

// KGNodeType identifies what kind of entity a knowledge graph node is.
type KGNodeType string

const (
	KGNodeConcept KGNodeType = "concept"
	KGNodeFile    KGNodeType = "file"
	KGNodeTool    KGNodeType = "tool"
	KGNodeAgent   KGNodeType = "agent"
	KGNodeTask    KGNodeType = "task"
	KGNodeFact    KGNodeType = "fact"
)

// KGRelationType identifies the relationship between two nodes.
type KGRelationType string

const (
	KGRelDependsOn  KGRelationType = "depends_on"
	KGRelRelatedTo  KGRelationType = "related_to"
	KGRelProducedBy KGRelationType = "produced_by"
	KGRelUsedBy     KGRelationType = "used_by"
	KGRelContains   KGRelationType = "contains"
	KGRelCausedBy   KGRelationType = "caused_by"
)

// KGNode is a node in the knowledge graph.
type KGNode struct {
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	Type       KGNodeType        `json:"type"`
	Properties map[string]string `json:"properties,omitempty"`
	Vector     []float32         `json:"vector,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// KGEdge is an edge (relationship) between two nodes.
type KGEdge struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Relation  KGRelationType `json:"relation"`
	Weight    float64        `json:"weight,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// ============================================================================
// CHANGE RECORDING — captures what each mutating tool did (before/after)
// ============================================================================

// ChangeKind classifies the nature of a recorded change.
type ChangeKind string

const (
	ChangeFileCreate ChangeKind = "file_create"
	ChangeFileModify ChangeKind = "file_modify"
	ChangeFileDelete ChangeKind = "file_delete"
	ChangeCommand    ChangeKind = "command"
	ChangeGit        ChangeKind = "git"
)

// Change records a single mutating action taken by a tool, capturing enough
// information to show the user exactly what was modified (before → after).
type Change struct {
	Tool      string     `json:"tool"`
	Kind      ChangeKind `json:"kind"`
	Path      string     `json:"path,omitempty"`      // file path (file ops)
	Before    string     `json:"before,omitempty"`    // previous file content
	After     string     `json:"after,omitempty"`     // new file content
	Command   string     `json:"command,omitempty"`   // shell command (terminal)
	Output    string     `json:"output,omitempty"`    // command output / summary
	ExitCode  int        `json:"exit_code,omitempty"` // terminal exit code
	Success   bool       `json:"success"`
	Timestamp time.Time  `json:"timestamp"`
}

// IsFileChange returns true if this change modified the filesystem.
func (c Change) IsFileChange() bool {
	return c.Kind == ChangeFileCreate || c.Kind == ChangeFileModify || c.Kind == ChangeFileDelete
}

// ParseRoutingMode safely converts a string to a RoutingMode.
func ParseRoutingMode(s string) RoutingMode {
	switch s {
	case "escalation":
		return RouteEscalation
	case "consensus":
		return RouteConsensus
	default:
		return RouteSingle
	}
}

// ParseModelTier safely converts a string to a ModelTier.
func ParseModelTier(s string) ModelTier {
	switch strings.ToLower(s) {
	case "reasoning":
		return ModelTierReasoning
	case "coding":
		return ModelTierCoding
	case "fast":
		return ModelTierFast
	case "local":
		return ModelTierLocal
	case "medium_local":
		return ModelTierMediumLocal
	case "tiny_local":
		return ModelTierTinyLocal
	case "critic":
		return ModelTierCritic
	default:
		return ModelTierCoding
	}
}

// PrimaryTierForMode returns the default primary tier for a given mode.
func PrimaryTierForMode(mode RoutingMode) ModelTier {
	switch mode {
	case RouteConsensus:
		return ModelTierReasoning
	case RouteEscalation:
		return ModelTierCoding
	default:
		return ModelTierCoding
	}
}
