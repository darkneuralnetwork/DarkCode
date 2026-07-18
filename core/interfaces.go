package core

import (
	"context"
	"errors"
)

// ErrContextTooLong is the sentinel every provider maps a context-overflow
// response to (llama-server's "context window exceeded", OpenAI's
// "context_length_exceeded", etc.), so callers can errors.Is it and react —
// re-fit the prompt smaller and retry, or fall back to a larger-window model —
// instead of treating a recoverable overflow as a fatal error. The
// deterministic FitToWindow guarantee at every dispatch site makes this rare;
// this is the belt-and-suspenders for tokenizer-estimate drift.
var ErrContextTooLong = errors.New("context window exceeded")

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

// TaskLoRA maps an auxiliary task to the LoRA adapter that specializes the
// local model for it. Only the summarizer was wired before; coding and
// planner adapters were downloaded but never mounted (dead weight). The
// llama-server hot-swap (scale 0↔1, no restart) is already implemented — this
// registry is what actually routes a task to its adapter.
var TaskLoRA = map[string]string{
	"local_compression": "summarizer",
	"summarize":         "summarizer",
	"coding":            "coding",
	"planning":          "planner",
}

// WithLoRA mounts the adapter registered for task (scale 1.0), runs fn, then
// unmounts it (scale 0.0) — the single audited path for per-task LoRA
// switching. A client that isn't a LoRAManager, an unknown task, or a mount
// failure all fall through to running fn on the base model: LoRA is an
// enhancement, never a hard dependency. Unlike the old inline `_ =` mounts,
// mount failures are RETURNED via the logger so a missing/misplaced adapter is
// visible instead of silently degrading. logf may be nil.
func WithLoRA(client LLMClient, task string, logf func(format string, args ...interface{}), fn func() error) error {
	name, ok := TaskLoRA[task]
	if !ok {
		return fn()
	}
	lm, ok := client.(LoRAManager)
	if !ok {
		return fn() // cloud or non-LoRA client — base model
	}
	if err := lm.MountLoRA(name, 1.0); err != nil {
		if logf != nil {
			logf("lora mount %q for task %q failed, using base model: %v", name, task, err)
		}
		return fn() // base-model fallthrough, never fatal
	}
	defer func() {
		if err := lm.MountLoRA(name, 0.0); err != nil && logf != nil {
			logf("lora unmount %q failed: %v", name, err)
		}
	}()
	return fn()
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
	AddEdge(edge *KGEdge) error
	GetNode(id string) (*KGNode, bool)
	Relate(fromID, toID string, relation KGRelationType) error
	RecordWordRelations(text string) error
	AllNodes() []*KGNode
	FindByType(t KGNodeType) []*KGNode
	GetEdges(id string) []*KGEdge
	AllEdges() []*KGEdge
	Stats() (int, int)
	ConceptRelations(concept string) interface{}
	// AdjustConfidence changes a node's stored Confidence by delta, clamped
	// to [floor, 1.0]. Returns the new confidence and whether the node was
	// found. Used to demote a fact (local-first upgrade Phase D hardening:
	// write-back governance) when real usage shows it was wrong — see
	// orchestrator/cascade.go's detectReAsk.
	AdjustConfidence(id string, delta, floor float64) (float64, bool)
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
