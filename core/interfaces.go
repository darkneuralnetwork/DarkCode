package core

import "context"

// ============================================================================
// INTERFACES — Decouples the kernel from concrete implementations so that
// each layer can be tested in isolation with mocks.
// ============================================================================

// ModelRouter abstracts model selection and multi-model orchestration (Layer 2).
type ModelRouter interface {
	// Route selects the best LLM client + model name for the given tier,
	// complexity score, and goal description.
	Route(tier ModelTier, complexity int, desc string) (LLMClient, string, error)

	// Consensus fans out to all non-primary models and synthesizes a unified
	// answer via the primary model.
	Consensus(ctx context.Context, msgs []Message, goal string) (*ConsensusResult, error)

	// GetMode returns the current routing mode (single/escalation/consensus).
	GetMode() RoutingMode

	// ModelCount returns how many models are currently registered.
	ModelCount() int
}

// LLMClient abstracts an LLM API client. Implemented by provider clients.
type LLMClient interface {
	ChatCompletion(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req *CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error)
	CreateEmbedding(ctx context.Context, text string) ([]float32, error)
	ModelInfo() ModelMetadata
	Ping(ctx context.Context) error
	Close() error
}

// LoRAManager provides capabilities to dynamically mount/unmount LoRA adapters
// on local LLM providers that support it (e.g. llama-server).
type LoRAManager interface {
	MountLoRA(name string, scale float32) error
}

// ContextCompressor abstracts context compression (Layer 3).
type ContextCompressor interface {
	// Compress reduces a conversation history to a structured snapshot.
	Compress(ctx context.Context, messages []Message, goal string) (*ContextSnapshot, error)

	// CompressBlock compresses a single block of messages (hierarchical compression).
	CompressBlock(ctx context.Context, messages []Message, goal string) (*CompressedBlock, error)

	// Summarize produces a concise narrative markdown briefing of arbitrary text.
	Summarize(ctx context.Context, text, focus string) (string, error)

	// IsEnabled returns whether compression is active.
	IsEnabled() bool

	// AssembleContext builds the final LLM context from STM messages within a
	// token budget, compressing as needed.
	AssembleContext(ctx context.Context, messages []Message, goal string, tokenBudget int) ([]Message, error)
}

// MemoryStore abstracts the unified memory system (Layer 4).
type MemoryStore interface {
	SetEmbedder(client LLMClient)
	GetEmbedding(text string) ([]float32, error)

	// STM operations
	STMAdd(msg Message)
	STMGet() []Message
	STMClear()
	STMCompress(briefing []Message, keepRecent int)

	// Episodic
	EpisodicAdd(entry EpisodicEntry) error
	EpisodicGet() []EpisodicEntry
	EpisodicSearch(query string) []EpisodicEntry

	// Semantic
	SemanticAdd(key, content, category string, tags []string) error
	SemanticAll() []*SemanticEntry

	// Procedural
	ProceduralAdd(skill *Skill) error
	ProceduralGet(name string) (*Skill, bool)
	ProceduralAll() []*Skill

	// Sub-systems
	KG() KnowledgeGraphStore
	Learning() LearningStore
	Audit() AuditStore

	// Summary
	Summary() string
}

// KnowledgeGraphStore abstracts the knowledge graph.
type KnowledgeGraphStore interface {
	AddNode(node *KGNode) error
	Relate(fromID, toID string, relation KGRelationType) error
	RecordWordRelations(text string) error
	AllNodes() []*KGNode
	FindByType(t KGNodeType) []*KGNode
	GetEdges(id string) []*KGEdge
	AllEdges() []*KGEdge
	Stats() (int, int)
	ConceptRelations(concept string) interface{}
}

// LearningStore abstracts the learning engine.
type LearningStore interface {
	RecordFeedback(fb LearningFeedback) error
	GetStats() map[string]interface{}
	GetAllStrategies() []*LearnedStrategy
}

// AuditStore abstracts the audit trail.
type AuditStore interface {
	RecordAction(agent AgentRole, strategy, tools string, risk RiskLevel, approved bool, outcome string) error
	GetAll() []AuditEntry
	GetRecent(n int) []AuditEntry
	Summary() map[string]interface{}
}

// ToolRegistry abstracts tool management and dispatch (Layer 6).
type ToolRegistry interface {
	DispatchAll(ctx context.Context, calls []ToolCall) interface{}
	LLMSchemas() interface{}
}

// ============================================================================
// COMPRESSED BLOCK — Used by hierarchical compression (Priority 1)
// ============================================================================

// CompressedBlock represents a compressed block of conversation messages.
// The hierarchical compression system divides STM into blocks of ~6-8
// messages and compresses each block independently. Blocks are cached and
// never re-compressed, preventing drift.
type CompressedBlock struct {
	// BlockID is a unique identifier for this block.
	BlockID string `json:"block_id"`

	// OriginalRange is the [start, end) message indices that were compressed.
	OriginalRange [2]int `json:"original_range"`

	// Summary is the LLM-generated narrative summary of the block.
	Summary string `json:"summary"`

	// PinnedMessages are high-importance messages kept verbatim.
	PinnedMessages []Message `json:"pinned_messages,omitempty"`

	// ToolSnapshots are preserved tool call results.
	ToolSnapshots []ToolSnapshot `json:"tool_snapshots,omitempty"`

	// EstimatedTokens is the token count of this compressed block.
	EstimatedTokens int `json:"estimated_tokens"`

	// CompressedAt is when this block was compressed.
	CompressedAt string `json:"compressed_at"`
}

// ToolSnapshot preserves key information from a tool call result so that
// the agent remembers what actions it took even after compression.
type ToolSnapshot struct {
	Name   string `json:"name"`
	Args   string `json:"args"`   // abbreviated arguments
	Output string `json:"output"` // first ~200 chars + key findings
	Path   string `json:"path"`   // if file-related
	Status bool   `json:"status"`
}
