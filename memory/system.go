package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/observability"
)

// ============================================================================
// LAYER 4 — EXPANDED MEMORY SYSTEM
// Four memory types: Short-Term, Episodic, Semantic, Procedural (Skills)
// ============================================================================

// System is the unified memory system managing all memory types.
type System struct {
	mu       sync.RWMutex
	dataDir  string
	embedder core.LLMClient

	// Short-Term Memory — active conversation window (in-memory only)
	stm    []core.Message
	stmMax int
	// transcript holds every turn that has left the STM window, in order,
	// whether it was pushed out by the size cap or replaced by a compaction
	// briefing. The window is what the model is shown; this is what was said.
	//
	// Both paths used to just drop the overflow, so a long session silently
	// lost its own beginning and neither the model, /log, nor the user could
	// get it back. In memory only and not persisted: it is for answering
	// questions within a session, not a durable record — episodic memory is
	// that. Capped by transcriptMax so a very long session cannot grow
	// without bound.
	transcript []core.Message

	// sessionEpoch marks the start of the current chat session. Episodic
	// recall ignores conversation entries older than this so a "New Chat"
	// gives a clean conversational slate without deleting long-term memory.
	// Bumped by StartNewSession (wired to /api/reset and CLI /new).
	sessionEpoch time.Time

	// Episodic Memory — past task executions
	episodic       []core.EpisodicEntry
	episodicPath   string
	episodicWriter *DebouncedWriter

	// Semantic Memory — knowledge/docs/code (keyword-indexed)
	semantic       map[string]*core.SemanticEntry
	semanticPath   string
	semanticWriter *DebouncedWriter

	// Procedural Memory — reusable skills
	procedural       map[string]*core.Skill
	proceduralPath   string
	proceduralWriter *DebouncedWriter

	// Knowledge Graph — entity-relationship store
	knowledgeGraph *KnowledgeGraph

	// Learning Engine — task outcome analysis
	learningEngine *LearningEngine

	// Audit Log — action audit trail
	auditLog *AuditLog

	// Architecture — persisted knowledge of codebase design decisions
	// (component map, dependency notes, constraints) recorded by the agent
	// across a session. Writes should go through ArchitectureAddDecision so
	// they're persisted; see memory/layers.go for why this is the only
	// surviving "Phase 9" memory tier.
	Architecture       *ArchitectureMemory
	architecturePath   string
	architectureWriter *DebouncedWriter

	// embedWG tracks vector backfills still in flight. Embedding moved off the
	// write path so a task's completion is not delayed by a network call, which
	// means a vector can still be arriving when the process is asked to stop —
	// and a backfill lost at that point is a vector that never comes back,
	// because nothing re-embeds an existing entry. Shutdown waits on this.
	embedWG sync.WaitGroup
}

// NewSystem creates a unified memory system rooted at the given directory.
func NewSystem(dataDir string) (*System, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create memory dir: %w", err)
	}

	s := &System{
		dataDir:          dataDir,
		stmMax:           50, // keep last 50 messages in STM
		episodicPath:     filepath.Join(dataDir, "episodic.json"),
		semanticPath:     filepath.Join(dataDir, "semantic.json"),
		proceduralPath:   filepath.Join(dataDir, "procedural.json"),
		architecturePath: filepath.Join(dataDir, "architecture.json"),
		semantic:         make(map[string]*core.SemanticEntry),
		procedural:       make(map[string]*core.Skill),

		Architecture: NewArchitectureMemory(),
	}

	// Load persistent stores
	if err := s.loadEpisodic(); err != nil {
		return nil, err
	}
	if err := s.loadSemantic(); err != nil {
		return nil, err
	}
	if err := s.loadProcedural(); err != nil {
		return nil, err
	}
	if err := s.loadArchitecture(); err != nil {
		return nil, err
	}

	// Initialize Writers (after loading, so we don't overwrite with empty)
	s.episodicWriter = NewDebouncedWriter(s.episodicPath, 2*time.Second, func() ([]byte, error) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return json.Marshal(s.episodic) // using non-indent for speed
	})
	s.semanticWriter = NewDebouncedWriter(s.semanticPath, 2*time.Second, func() ([]byte, error) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return json.Marshal(s.semantic)
	})
	s.proceduralWriter = NewDebouncedWriter(s.proceduralPath, 2*time.Second, func() ([]byte, error) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return json.Marshal(s.procedural)
	})
	s.architectureWriter = NewDebouncedWriter(s.architecturePath, 2*time.Second, func() ([]byte, error) {
		s.Architecture.mu.RLock()
		defer s.Architecture.mu.RUnlock()
		return json.Marshal(s.Architecture.data)
	})

	// Initialize Knowledge Graph
	kg, err := NewKnowledgeGraph(dataDir)
	if err != nil {
		return nil, fmt.Errorf("init knowledge graph: %w", err)
	}
	s.knowledgeGraph = kg

	// Initialize Learning Engine
	le, err := NewLearningEngine(dataDir)
	if err != nil {
		return nil, fmt.Errorf("init learning engine: %w", err)
	}
	s.learningEngine = le

	// Initialize Audit Log
	al, err := NewAuditLog(dataDir)
	if err != nil {
		return nil, fmt.Errorf("init audit log: %w", err)
	}
	s.auditLog = al

	return s, nil
}

// WaitForEmbeddings blocks until every in-flight vector backfill has finished.
// Must not be called while holding s.mu — the backfills take it themselves.
func (s *System) WaitForEmbeddings() { s.embedWG.Wait() }

// Shutdown flushes all pending memory writes to disk.
func (s *System) Shutdown() {
	// Let in-flight vectors land before the writers flush, so a backfill that
	// was seconds from completing is persisted rather than dropped. Nothing
	// re-embeds an existing entry, so one lost here is lost permanently.
	s.WaitForEmbeddings()
	if s.episodicWriter != nil {
		s.episodicWriter.Shutdown()
	}
	if s.semanticWriter != nil {
		s.semanticWriter.Shutdown()
	}
	if s.proceduralWriter != nil {
		s.proceduralWriter.Shutdown()
	}
	if s.architectureWriter != nil {
		s.architectureWriter.Shutdown()
	}
	if s.knowledgeGraph != nil {
		s.knowledgeGraph.Shutdown()
	}
	if s.learningEngine != nil {
		s.learningEngine.Shutdown()
	}
	if s.auditLog != nil {
		s.auditLog.Shutdown()
	}
}

// SetEmbedder injects an LLMClient to be used for generating vector embeddings.
func (s *System) SetEmbedder(client core.LLMClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedder = client
	if s.knowledgeGraph != nil {
		s.knowledgeGraph.SetEmbedder(client)
	}
}

// embeddingTimeout bounds a single embedding call. GetEmbedding sits on hot
// paths (every HybridRetriever.Recall embeds the query; every episodic/
// semantic write embeds the entry) and the underlying llm.Client's HTTP
// timeout is minutes-long — a wedged local server must degrade recall to the
// keyword path quickly, not stall every request.
const embeddingTimeout = 5 * time.Second

// GetEmbedding generates a vector embedding using the registered embedder.
func (s *System) GetEmbedding(text string) ([]float32, error) {
	s.mu.RLock()
	client := s.embedder
	s.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("no embedder configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), embeddingTimeout)
	defer cancel()
	return client.CreateEmbedding(ctx, text)
}

// ============================================================================
// SHORT-TERM MEMORY (STM) — in-memory only, rolling window
// ============================================================================

// STMAdd adds a message to short-term memory.
func (s *System) STMAdd(msg core.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stm = append(s.stm, msg)
	// Trim to max size, keeping most recent. What falls off the front is moved
	// to the transcript rather than discarded — see the field comment.
	if len(s.stm) > s.stmMax {
		drop := len(s.stm) - s.stmMax
		s.transcript = append(s.transcript, s.stm[:drop]...)
		s.stm = s.stm[drop:]
		s.trimTranscriptLocked()
	}
}

// transcriptMax bounds the retained history. Generous, because a message is
// small and losing the start of a session is the failure this exists to
// prevent, but finite so a days-long session cannot grow without bound.
const transcriptMax = 2000

// trimTranscriptLocked drops the oldest retained turns past the cap. Caller
// holds s.mu.
func (s *System) trimTranscriptLocked() {
	if len(s.transcript) > transcriptMax {
		s.transcript = s.transcript[len(s.transcript)-transcriptMax:]
	}
}

// STMTranscript returns every turn that has left the active window, oldest
// first. Use it to answer "what did we say earlier" after a compaction has
// replaced those turns with a briefing.
func (s *System) STMTranscript() []core.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.Message, len(s.transcript))
	copy(out, s.transcript)
	return out
}

// STMFullHistory returns the whole conversation as it was actually said:
// everything that has left the window, followed by everything still in it.
func (s *System) STMFullHistory() []core.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.Message, 0, len(s.transcript)+len(s.stm))
	out = append(out, s.transcript...)
	out = append(out, s.stm...)
	return out
}

// STMGet returns the short-term memory messages.
func (s *System) STMGet() []core.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]core.Message, len(s.stm))
	copy(result, s.stm)
	return result
}

// STMClear clears short-term memory (e.g., at start of a new task).
func (s *System) STMClear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stm = s.stm[:0]
	// The retained transcript goes with it. It exists so a compaction cannot
	// lose the earlier part of THIS conversation; carrying it past a reset
	// would let a new chat recall the previous one, which is the isolation
	// StartNewSession and the session epoch exist to provide.
	s.transcript = s.transcript[:0]
}

// STMTruncate drops all but the first n messages. A checkpoint rollback uses
// it to rewind the transcript to the point the filesystem was restored to —
// without it the agent keeps reasoning from turns that describe files that no
// longer exist. Growing is not possible, so n beyond the current length is a
// no-op.
func (s *System) STMTruncate(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n < len(s.stm) {
		s.stm = s.stm[:n]
	}
}

// StartNewSession begins a fresh chat session: it clears short-term memory and
// advances the session epoch so episodic recall stops surfacing prior-session
// conversations. Durable semantic/KG/procedural memory is deliberately kept —
// this is a conversational reset, not a memory wipe. Wired to /api/reset and
// the CLI /new command.
func (s *System) StartNewSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stm = s.stm[:0]
	s.sessionEpoch = time.Now()
}

// SessionEpoch returns the start time of the current chat session (zero if a
// fresh session was never started). Recall uses it to exclude conversation
// entries from before the current session.
func (s *System) SessionEpoch() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionEpoch
}

// STMSetMax adjusts the STM window size.
func (s *System) STMSetMax(max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmMax = max
	if len(s.stm) > max {
		s.stm = s.stm[len(s.stm)-max:]
	}
}

// STMCompress replaces the older portion of short-term memory with a
// compressed briefing (the snapshot converted to messages by the caller) plus
// the most recent `keepRecent` messages for conversational continuity.
//
// This makes context compression actually shrink the conversation for ALL
// modes (general, project, loop): instead of hard-truncating old messages at
// stmMax (data loss), the compressed briefing preserves the gist across the
// boundary. Previously the compressor produced a ContextSnapshot but
// SnapshotToMessages was never called, so compression cost an LLM call yet
// did nothing — the full raw STM was always appended.
//
// Layout after compression: [compressedBriefing...] + [last keepRecent msgs].
// If keepRecent >= len(stm), the recent tail is the whole STM (no loss beyond
// the compression itself). The caller passes already-converted messages so
// this package stays decoupled from compression.
func (s *System) STMCompress(briefing []core.Message, keepRecent int) {
	if len(briefing) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if keepRecent < 0 {
		keepRecent = 0
	}
	if keepRecent > len(s.stm) {
		keepRecent = len(s.stm)
	}

	// Flush the originals BEFORE replacing them. Compaction changes what the
	// model is shown; it must not delete what was said. Only the turns leaving
	// the window are recorded — the retained tail is still in stm and would
	// otherwise be stored twice.
	//
	// This used to overwrite the buffer outright and the replaced turns were
	// gone for good: from the model, from /log, and from any later question
	// about what happened earlier in the session. Compaction fires mid-task,
	// and an agent that summarised its own working memory and then needs a
	// detail out of it had no way back. Read it with STMTranscript.
	if drop := len(s.stm) - keepRecent; drop > 0 {
		s.transcript = append(s.transcript, s.stm[:drop]...)
		s.trimTranscriptLocked()
	}

	tail := s.stm[len(s.stm)-keepRecent:]
	merged := make([]core.Message, 0, len(briefing)+keepRecent)
	merged = append(merged, briefing...)
	merged = append(merged, tail...)
	// Respect stmMax as a hard ceiling so a very large briefing can't blow
	// out the window.
	if len(merged) > s.stmMax {
		merged = merged[len(merged)-s.stmMax:]
	}
	s.stm = merged
}

// ============================================================================
// EPISODIC MEMORY — persistent task execution logs
// ============================================================================

// EpisodicAdd records a completed task execution.
//
// The vector is filled in afterwards, not before the write. This used to embed
// inline, which put a network call with a five-second timeout between the user
// and the end of their request — every completed task paid it, and a wedged
// embedder turned every single turn into a five-second stall for a field that
// only improves ranking.
//
// An entry with no vector is not broken, it is the same entry every install
// without an embedder already has: recall falls back to keyword overlap until
// the vector lands. Degrading a ranking signal is a much smaller cost than
// delaying the answer.
func (s *System) EpisodicAdd(entry core.EpisodicEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.ID == "" {
		entry.ID = fmt.Sprintf("ep_%d_%d", time.Now().Unix(), len(s.episodic))
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	// Stamp the answer-cache admission class here, at the single choke point
	// every write passes through, so no execution path can add an entry that
	// skipped classification. A caller may pre-set it; otherwise it is derived
	// from the entry itself (replay.go).
	if entry.Replay == "" {
		entry.Replay = ClassifyReplay(entry)
	}
	s.episodic = append(s.episodic, entry)
	s.episodicWriter.MarkDirty()
	s.embedEpisodicLater(entry.ID, entry.TaskGoal+" "+entry.Summary)
	return nil
}

// embedEpisodicLater computes an entry's vector off the write path and fills it
// in when it arrives. Must be called with s.mu held.
//
// No-op without an embedder, so an install that never configured one spawns
// nothing at all. Panic-guarded: this runs on its own goroutine, where an
// unrecovered panic ends the process rather than the task.
func (s *System) embedEpisodicLater(id, text string) {
	if s.embedder == nil || id == "" {
		return
	}
	s.embedWG.Add(1)
	observability.Go("episodic-embed", func() {
		defer s.embedWG.Done()
		vec, err := s.GetEmbedding(text)
		if err != nil || len(vec) == 0 {
			return // keyword recall still works; this was only a ranking signal
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.episodic {
			if s.episodic[i].ID == id {
				s.episodic[i].Vector = vec
				s.episodicWriter.MarkDirty()
				return
			}
		}
	})
}

// EpisodicGet returns all episodic entries, most recent first.
func (s *System) EpisodicGet() []core.EpisodicEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]core.EpisodicEntry, len(s.episodic))
	copy(result, s.episodic)
	// Reverse for most-recent-first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// EpisodicSearch returns entries whose goal or summary contains the query.
func (s *System) EpisodicSearch(query string) []core.EpisodicEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	queryLower := strings.ToLower(query)
	var results []core.EpisodicEntry
	for _, entry := range s.episodic {
		if strings.Contains(strings.ToLower(entry.TaskGoal), queryLower) ||
			strings.Contains(strings.ToLower(entry.Summary), queryLower) {
			results = append(results, entry)
		}
	}
	return results
}

// EpisodicGetRecent returns the N most recent entries.
func (s *System) EpisodicGetRecent(n int) []core.EpisodicEntry {
	all := s.EpisodicGet()
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// EpisodicPrune drops entries older than cutoff and returns how many were
// removed. Episodic memory grows with every task and is the one tier where old
// entries stop earning their context cost, so it needs a way out.
func (s *System) EpisodicPrune(cutoff time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.episodic[:0]
	for _, e := range s.episodic {
		if e.Timestamp.After(cutoff) {
			kept = append(kept, e)
		}
	}
	removed := len(s.episodic) - len(kept)
	s.episodic = kept
	if removed > 0 {
		s.episodicWriter.MarkDirty()
	}
	return removed
}

// ============================================================================
// SEMANTIC MEMORY — knowledge, docs, code (keyword-indexed)
// ============================================================================

// SemanticAdd stores a knowledge entry.
func (s *System) SemanticAdd(key, content, category string, tags []string) error {
	vec, _ := s.GetEmbedding(key + " " + content)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.semantic[key] = &core.SemanticEntry{
		Key:       key,
		Content:   content,
		Category:  category,
		Tags:      tags,
		Vector:    vec,
		CreatedAt: time.Now(),
	}
	s.semanticWriter.MarkDirty()
	return nil
}

// SemanticGet retrieves a knowledge entry by key.
func (s *System) SemanticGet(key string) (*core.SemanticEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.semantic[key]
	return entry, ok
}

// SemanticSearch returns entries matching the query (in content, key, or tags).
func (s *System) SemanticSearch(query string) []*core.SemanticEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	queryLower := strings.ToLower(query)
	var results []*core.SemanticEntry
	for _, entry := range s.semantic {
		if strings.Contains(strings.ToLower(entry.Key), queryLower) ||
			strings.Contains(strings.ToLower(entry.Content), queryLower) {
			results = append(results, entry)
			continue
		}
		for _, tag := range entry.Tags {
			if strings.Contains(strings.ToLower(tag), queryLower) {
				results = append(results, entry)
				break
			}
		}
	}
	return results
}

// SemanticRemove deletes a knowledge entry.
func (s *System) SemanticRemove(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.semantic, key)
	s.semanticWriter.MarkDirty()
	return nil
}

// SemanticAll returns all semantic entries.
func (s *System) SemanticAll() []*core.SemanticEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*core.SemanticEntry, 0, len(s.semantic))
	for _, entry := range s.semantic {
		result = append(result, entry)
	}
	return result
}

// ============================================================================
// PROCEDURAL MEMORY — reusable skills/workflows
// ============================================================================

// FlushProcedural writes procedural memory to disk immediately, for callers
// that cannot wait out the debounce — a bulk import, or a process about to end.
func (s *System) FlushProcedural() {
	if s.proceduralWriter != nil {
		s.proceduralWriter.FlushNow()
	}
}

// ProceduralAdd stores a new skill.
func (s *System) ProceduralAdd(skill *core.Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if skill.CreatedAt.IsZero() {
		skill.CreatedAt = time.Now()
	}
	s.procedural[skill.Name] = skill
	s.proceduralWriter.MarkDirty()
	return nil
}

// ProceduralGet retrieves a skill by name.
func (s *System) ProceduralGet(name string) (*core.Skill, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	skill, ok := s.procedural[name]
	return skill, ok
}

// ProceduralSearch returns skills whose name or description matches.
func (s *System) ProceduralSearch(query string) []*core.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	queryLower := strings.ToLower(query)
	var results []*core.Skill
	for _, skill := range s.procedural {
		if strings.Contains(strings.ToLower(skill.Name), queryLower) ||
			strings.Contains(strings.ToLower(skill.Description), queryLower) {
			results = append(results, skill)
		}
	}
	return results
}

// ProceduralAll returns all skills, sorted by use count (most used first).
func (s *System) ProceduralAll() []*core.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*core.Skill, 0, len(s.procedural))
	for _, skill := range s.procedural {
		result = append(result, skill)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].UseCount > result[j].UseCount
	})
	return result
}

// ProceduralMarkUsed updates a skill's usage statistics.
func (s *System) ProceduralMarkUsed(name string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	skill, ok := s.procedural[name]
	if !ok {
		return fmt.Errorf("skill %s not found", name)
	}

	skill.UseCount++
	now := time.Now()
	skill.LastUsed = &now

	// Update rolling success rate
	if success {
		skill.SuccessRate = (skill.SuccessRate*float64(skill.UseCount-1) + 1.0) / float64(skill.UseCount)
	} else {
		skill.SuccessRate = (skill.SuccessRate * float64(skill.UseCount-1)) / float64(skill.UseCount)
	}

	s.proceduralWriter.MarkDirty()
	return nil
}

// ProceduralRemove deletes a skill.
func (s *System) ProceduralRemove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.procedural, name)
	s.proceduralWriter.MarkDirty()
	return nil
}

// ============================================================================
// PERSISTENCE — load/save to disk
// ============================================================================

func (s *System) loadEpisodic() error {
	data, err := os.ReadFile(s.episodicPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh start
		}
		return err
	}
	return json.Unmarshal(data, &s.episodic)
}

func (s *System) loadSemantic() error {
	data, err := os.ReadFile(s.semanticPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.semantic)
}

func (s *System) loadProcedural() error {
	data, err := os.ReadFile(s.proceduralPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.procedural)
}

func (s *System) loadArchitecture() error {
	data, err := os.ReadFile(s.architecturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	s.Architecture.mu.Lock()
	s.Architecture.data = m
	s.Architecture.mu.Unlock()
	return nil
}

// ============================================================================
// ARCHITECTURE MEMORY — persisted design-decision log
// ============================================================================

// ArchitectureAddDecision records an architectural decision and persists it
// to disk (architecture.json). Prefer this over Architecture.AddDecision
// directly, which doesn't trigger a write.
func (s *System) ArchitectureAddDecision(title, decision string) {
	s.Architecture.AddDecision(title, decision)
	s.architectureWriter.MarkDirty()
}

// ArchitectureDecisions returns all recorded architectural decisions.
func (s *System) ArchitectureDecisions() map[string]string {
	return s.Architecture.Decisions()
}

// ============================================================================
// SUMMARY
// ============================================================================

// Summary returns a summary of all memory systems.
func (s *System) Summary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeCount, edgeCount := 0, 0
	if s.knowledgeGraph != nil {
		nodeCount, edgeCount = s.knowledgeGraph.Stats()
	}

	auditCount := 0
	if s.auditLog != nil {
		auditCount = s.auditLog.Count()
	}

	strategyCount := 0
	if s.learningEngine != nil {
		strategyCount = len(s.learningEngine.GetAllStrategies())
	}

	return fmt.Sprintf(
		"Memory System:\n"+
			"  STM: %d messages (max %d)\n"+
			"  Episodic: %d entries\n"+
			"  Semantic: %d entries\n"+
			"  Procedural: %d skills\n"+
			"  Knowledge Graph: %d nodes, %d edges\n"+
			"  Learned Strategies: %d\n"+
			"  Audit Log: %d entries",
		len(s.stm), s.stmMax,
		len(s.episodic),
		len(s.semantic),
		len(s.procedural),
		nodeCount, edgeCount,
		strategyCount,
		auditCount,
	)
}

// ============================================================================
// KNOWLEDGE GRAPH ACCESSORS
// ============================================================================

// ShortSummary returns a compact one-line memory overview for banners/status bars.
func (s *System) ShortSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodeCount, edgeCount := 0, 0
	if s.knowledgeGraph != nil {
		nodeCount, edgeCount = s.knowledgeGraph.Stats()
	}
	return fmt.Sprintf("STM:%d epi:%d sem:%d skills:%d kg:%d/%d",
		len(s.stm), len(s.episodic), len(s.semantic), len(s.procedural), nodeCount, edgeCount)
}

// KG returns the knowledge graph.
func (s *System) KG() core.KnowledgeGraphStore { return s.knowledgeGraph }

// ============================================================================
// LEARNING ENGINE ACCESSORS
// ============================================================================

// Learning returns the learning engine.
func (s *System) Learning() core.LearningStore { return s.learningEngine }

// ============================================================================
// AUDIT LOG ACCESSORS
// ============================================================================

// Audit returns the audit log.
func (s *System) Audit() core.AuditStore { return s.auditLog }
