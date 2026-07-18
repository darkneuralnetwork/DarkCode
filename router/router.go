package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/capability"
	"github.com/darkcode/compression"
	"github.com/darkcode/core"
	"github.com/darkcode/ui"
)

// RegisteredModel is a single registered model with its client, tier, and
// consensus role. The router keeps a slice of these (in addition to the
// legacy per-tier map) so that consensus mode can fan out to ALL registered
// models in parallel — the tier map alone cannot represent multiple models
// at the same tier (they would clobber each other).
type RegisteredModel struct {
	Name      string
	Client    core.LLMClient
	Tier      core.ModelTier
	Role      string // consensus role: critic, skeptic, knowledge_booster, ...
	IsPrimary bool

	// DisabledUntil temporarily takes this model out of routing/consensus
	// selection when set to a future time — zero value means enabled.
	// Lazy expiry: no background timer is needed, the model becomes
	// selectable again the moment time.Now() passes DisabledUntil (see
	// isModelDisabledLocked). See DisableModel/EnableModel.
	DisabledUntil time.Time
}

// Router is Layer 2 — the multi-model routing system.
// It selects models dynamically based on task complexity and supports
// three modes: single, escalation, and consensus.
type Router struct {
	mu                  sync.RWMutex
	clients             map[core.ModelTier]core.LLMClient // one client per tier (legacy, for Route/escalation)
	models              map[core.ModelTier]string         // model name per tier (legacy)
	allModels           []RegisteredModel                 // ALL registered models (for consensus fan-out)
	mode                core.RoutingMode
	emitter             *ui.EventEmitter
	escalationThreshold int // complexity score above which to escalate (0-10)

	// Phase 5 Multi-Model Routing Enhancements
	classifier   *TaskClassifier
	roleTracker  *RoleTracker
	roleSelector *RoleSelector
	modelPool    *TieredModelPool

	// Capability advisor (spec §1 wiring): influences local-vs-cloud
	// preference and concurrency based on detected hardware tier.
	advisor       *capability.Advisor

	// sequentialConsensus, when true, makes Consensus() call each model one
	// at a time instead of fanning out in parallel. Set by the kernel from the
	// resolved execution profile (Sequential mode, or Auto when only free-tier
	// cloud models are registered) so free-tier models with strict RPM limits
	// don't get hammered in parallel and trip 429s.
	sequentialConsensus bool

	enableLocalOffloading bool

	// forceLocal pins routing to the local model family (LocalMode "force"):
	// selectBestAvailable considers ONLY local tiers and returns an empty
	// tier (→ "no model available" error) when none is registered, so a
	// force-local request fails loudly instead of silently using a cloud
	// provider. Consensus fan-out is likewise restricted to local models.
	// Set from config by the kernel/wireup; see SetForceLocal.
	forceLocal bool
}

// localTiers is the local model family, best (largest) first. Shared by the
// force-local selection path and the prefer-local advisor path.
var localTiers = []core.ModelTier{
	core.ModelTierLocal, core.ModelTierMediumLocal, core.ModelTierTinyLocal,
}

// isLocalTier reports whether t is one of the local model tiers.
func isLocalTier(t core.ModelTier) bool {
	for _, lt := range localTiers {
		if t == lt {
			return true
		}
	}
	return false
}

// NewRouter creates a model router with the given model configurations.
func NewRouter(mode core.RoutingMode, emitter *ui.EventEmitter) *Router {
	return &Router{
		clients:             make(map[core.ModelTier]core.LLMClient),
		models:              make(map[core.ModelTier]string),
		mode:                mode,
		emitter:             emitter,
		escalationThreshold: 7,
		classifier:          NewTaskClassifier(),
		roleTracker:         NewRoleTracker(),
		roleSelector:        NewRoleSelector(),
		modelPool:           NewTieredModelPool(),
	}
}

// SetAdvisor attaches a capability advisor so routing decisions can account
// for hardware capabilities (e.g. prefer local models when the system is
// powerful enough). Safe to call with nil to disable.
func (r *Router) SetAdvisor(a *capability.Advisor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.advisor = a
}

// SetEnableLocalOffloading toggles whether specific simple tasks should be offloaded to local models.
func (r *Router) SetEnableLocalOffloading(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enableLocalOffloading = enabled
}

// SetForceLocal pins (or unpins) routing to the local model family. When
// enabled, no routing decision may select a cloud tier — see the forceLocal
// field and selectBestAvailable. Safe to call at runtime (e.g. from the
// /local force command or the GUI toggle) so the change takes effect on the
// next request without a restart.
func (r *Router) SetForceLocal(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forceLocal = enabled
}

// ForceLocal reports whether routing is currently pinned to local models.
func (r *Router) ForceLocal() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.forceLocal
}

// RegisterModel associates a model tier with a client and model name. It
// updates both the legacy per-tier map (used by Route/escalation) and the
// allModels slice (used by consensus fan-out). If a model with the same name
// is already registered, its client/tier are updated in place (dedup by name)
// so that re-registration during hot-reload does not create duplicates.
func (r *Router) RegisterModel(tier core.ModelTier, client core.LLMClient, modelName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[tier] = client
	r.models[tier] = modelName

	// Upsert into allModels (dedup by name).
	for i := range r.allModels {
		if r.allModels[i].Name == modelName {
			r.allModels[i].Client = client
			r.allModels[i].Tier = tier
			return
		}
	}
	r.allModels = append(r.allModels, RegisteredModel{
		Name:   modelName,
		Client: client,
		Tier:   tier,
	})
}

// MarkPrimary flags the named model as the primary (consensus synthesizer).
// All other models are marked non-primary. Called after RegisterModel during
// startup and hot-reload so the Consensus method knows which model synthesizes.
func (r *Router) MarkPrimary(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.allModels {
		r.allModels[i].IsPrimary = r.allModels[i].Name == name
	}
}

// SetModelRole sets the consensus role for the named model.
func (r *Router) SetModelRole(name, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.allModels {
		if r.allModels[i].Name == name {
			r.allModels[i].Role = role
			return
		}
	}
}

// DisableModel temporarily takes the named model out of routing/consensus
// selection until the given time (local-first upgrade §6c). Route/
// selectBestAvailable route around it (falling through to the next
// available tier) and Consensus excludes it from the contributor fan-out.
// A no-op if no model with that name is registered.
func (r *Router) DisableModel(name string, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.allModels {
		if r.allModels[i].Name == name {
			r.allModels[i].DisabledUntil = until
			return
		}
	}
}

// EnableModel clears a temporary disable, making the model immediately
// selectable again. A no-op if the model isn't registered or wasn't
// disabled. Also happens automatically (lazy expiry, no call needed) once
// the DisableModel duration elapses — this is only for reversing it early.
func (r *Router) EnableModel(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.allModels {
		if r.allModels[i].Name == name {
			r.allModels[i].DisabledUntil = time.Time{}
			return
		}
	}
}

// isModelDisabledLocked reports whether the named model is currently
// temporarily disabled. Must be called with r.mu held (a read lock
// suffices). Lazy expiry: a past DisabledUntil means the model is enabled
// again, no separate re-enable step required.
func (r *Router) isModelDisabledLocked(name string) bool {
	for _, m := range r.allModels {
		if m.Name == name {
			return !m.DisabledUntil.IsZero() && time.Now().Before(m.DisabledUntil)
		}
	}
	return false
}

// IsModelDisabled is the exported, lock-safe form of isModelDisabledLocked,
// for callers (CLI/GUI status displays) outside the router package.
func (r *Router) IsModelDisabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isModelDisabledLocked(name)
}

// AllModels returns a snapshot of all registered models.
func (r *Router) AllModels() []RegisteredModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RegisteredModel, len(r.allModels))
	copy(out, r.allModels)
	return out
}

// ModelCount returns the number of registered models.
func (r *Router) ModelCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.allModels)
}

// SetMode changes the routing mode.
func (r *Router) SetMode(mode core.RoutingMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
}

// SetSequentialConsensus toggles whether Consensus() fans out to models in
// parallel (false, the default) or calls them one at a time (true). The
// kernel sets this from the resolved execution profile.
func (r *Router) SetSequentialConsensus(seq bool) {
	r.mu.Lock()
	r.sequentialConsensus = seq
	r.mu.Unlock()
}

// GetMode returns the current routing mode.
func (r *Router) GetMode() core.RoutingMode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mode
}

// HasModel checks if a specific tier is configured.
func (r *Router) HasModel(tier core.ModelTier) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.clients[tier]
	return ok
}

// HasLocalModel reports whether any local-tier model is currently registered.
// Used by force-local orchestration to decide whether the embedded model
// still needs to be brought up.
func (r *Router) HasLocalModel() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range localTiers {
		if _, ok := r.clients[t]; ok {
			return true
		}
	}
	return false
}

// Route selects the appropriate model for a task and returns the client + model name.
// In single mode, always returns the best available model.
// In escalation mode, returns a fast model unless complexity is high.
// In consensus mode, returns the reasoning model (consensus is handled separately).
func (r *Router) Route(tier core.ModelTier, complexity int, taskDesc string) (core.LLMClient, string, error) {
	r.mu.RLock()
	mode := r.mode
	threshold := r.escalationThreshold
	enableLocalOffload := r.enableLocalOffloading
	r.mu.RUnlock()

	var selectedTier core.ModelTier

	// --- Task-Specific Routing Intercept ---
	taskType := r.classifier.Classify(taskDesc)
	if enableLocalOffload {
		if taskType == TaskTypeTinyLocal {
			selectedTier = r.selectBestAvailable(core.ModelTierTinyLocal)
		} else if taskType == TaskTypeMediumLocal {
			selectedTier = r.selectBestAvailable(core.ModelTierMediumLocal)
		}
	}

	if selectedTier == "" {
		switch mode {
		case core.RouteSingle:
			// Use the requested tier, or fall back to any available
			selectedTier = r.selectBestAvailable(tier)

		case core.RouteEscalation:
			if complexity >= threshold {
				// Escalate to reasoning model
				selectedTier = r.selectBestAvailable(core.ModelTierReasoning)
			} else {
				// Use fast model for simple tasks
				selectedTier = r.selectBestAvailable(core.ModelTierFast)
			}

		case core.RouteConsensus:
			// In consensus mode, the primary is always reasoning tier
			selectedTier = r.selectBestAvailable(core.ModelTierReasoning)

		default:
			selectedTier = r.selectBestAvailable(tier)
		}
	}

	r.mu.RLock()
	client := r.clients[selectedTier]
	modelName := r.models[selectedTier]
	forceLocal := r.forceLocal
	r.mu.RUnlock()

	if client == nil {
		if forceLocal {
			// Force-local hard guarantee: never silently fall back to a cloud
			// provider. Say so explicitly so the user knows local mode is
			// active and what to do.
			return nil, "", fmt.Errorf("force-local mode is active but no local model is available — start/enable the local LLM (or run '/local off' to allow cloud models)")
		}
		return nil, "", fmt.Errorf("no model available for tier %s", selectedTier)
	}

	// Emit routing decision
	if r.emitter != nil {
		r.emitter.EmitModelRoute(selectedTier, mode,
			fmt.Sprintf("tier=%s complexity=%d model=%s", selectedTier, complexity, modelName))
		r.emitter.EmitTaskUpdate("router", "routing", fmt.Sprintf("Routed task (complexity: %d) to %s tier model: %s", complexity, selectedTier, modelName))
	}

	return client, modelName, nil
}

// selectBestAvailable returns the best available tier, trying the requested
// tier first, then falling back through the hierarchy. When a capability
// advisor is attached and recommends preferring local models, a local tier is
// tried before cloud tiers for non-reasoning tasks.
func (r *Router) selectBestAvailable(preferred core.ModelTier) core.ModelTier {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// If the advisor says "prefer local" and a local model is registered,
	// try local first (unless the caller explicitly wants reasoning/critic).
	// Consider the full local family: the primary local tier, plus the
	// medium/tiny local tiers a model may have been registered at based on its
	// size (previously only ModelTierLocal was checked, so a model registered
	// solely at medium_local/tiny_local was never preferred and could only be
	// reached via the exact-tier path — which the fallback hierarchy also
	// omitted).
	// tierAvailable reports whether tier has a registered client that isn't
	// temporarily disabled (local-first upgrade §6c). r.models[tier] holds
	// the name of whichever model currently occupies that tier slot — a
	// disabled model's tier is skipped exactly like an unregistered one, so
	// selection falls through to the next tier in the hierarchy rather than
	// erroring.
	tierAvailable := func(t core.ModelTier) bool {
		if _, ok := r.clients[t]; !ok {
			return false
		}
		return !r.isModelDisabledLocked(r.models[t])
	}

	// Force-local: routing is pinned to the local model family. Consider ONLY
	// local tiers and NEVER fall through to the cloud fallback hierarchy
	// below — when no local model is registered, return an empty tier so
	// Route()'s nil-client check yields a clear "no model available" error
	// instead of silently serving a remote model. This is the hard guarantee
	// behind LocalMode "force"; it overrides the preferred tier entirely
	// (even a reasoning/critic request stays local).
	if r.forceLocal {
		for _, t := range localTiers {
			if tierAvailable(t) {
				return t
			}
		}
		return core.ModelTier("")
	}

	if r.advisor != nil && r.advisor.PreferLocal() && preferred != core.ModelTierReasoning && preferred != core.ModelTierCritic {
		for _, t := range localTiers {
			if tierAvailable(t) {
				return t
			}
		}
	}

	// Try preferred tier
	if tierAvailable(preferred) {
		return preferred
	}

	// Fallback hierarchy: reasoning -> coding -> fast -> local (all sizes)
	// -> critic. medium_local/tiny_local are included so a non-primary local
	// model is reachable when the requested tier isn't available.
	fallbackOrder := []core.ModelTier{
		core.ModelTierReasoning,
		core.ModelTierCoding,
		core.ModelTierFast,
		core.ModelTierLocal,
		core.ModelTierMediumLocal,
		core.ModelTierTinyLocal,
		core.ModelTierCritic,
	}

	for _, tier := range fallbackOrder {
		if tierAvailable(tier) {
			return tier
		}
	}

	// Nothing available. If preferred is registered but every check above
	// rejected it, that can only mean it's temporarily disabled (§6c) — in
	// that case, deliberately return an empty (unregistered) tier so
	// Route()'s nil-client check produces a clear "no model available"
	// error instead of silently falling back to serving the disabled
	// model. If preferred was never registered at all, return it unchanged
	// (the original behavior — same clear error, different cause).
	if _, ok := r.clients[preferred]; ok {
		return core.ModelTier("")
	}
	return preferred
}

// Consensus runs all registered non-primary models in PARALLEL, each
// answering the query with its assigned role persona (critic, skeptic,
// knowledge_booster, …). The primary model then synthesizes all answers into
// a single consensus answer.
//
// This replaces the old sequential primary→critic→verify pipeline which (a)
// was sequential, (b) fell back to the primary model when no "critic" tier
// was registered (so the "critic" was the same model as the primary), and
// (c) was never reached for normal chat queries. The new design fans out to
// every registered model simultaneously, giving genuine multi-model consensus.
func (r *Router) Consensus(ctx context.Context, messages []core.Message, goal string) (*core.ConsensusResult, error) {
	r.mu.RLock()
	mode := r.mode
	forceLocal := r.forceLocal
	models := make([]RegisteredModel, len(r.allModels))
	copy(models, r.allModels)
	r.mu.RUnlock()

	if mode != core.RouteConsensus {
		return nil, fmt.Errorf("not in consensus mode")
	}

	// Force-local: restrict the consensus fan-out to local models so no cloud
	// provider is ever consulted while local mode is pinned. If that leaves
	// no models, fail loudly rather than silently widening back to cloud.
	if forceLocal {
		localOnly := models[:0:0]
		for _, m := range models {
			if isLocalTier(m.Tier) {
				localOnly = append(localOnly, m)
			}
		}
		if len(localOnly) == 0 {
			return nil, fmt.Errorf("force-local mode is active but no local model is registered for consensus — start/enable the local LLM (or run '/local off' to allow cloud models)")
		}
		models = localOnly
	}

	consensusStart := time.Now()

	// Identify the primary model and the contributing (non-primary) models.
	// Temporarily disabled models (local-first upgrade §6c) are excluded
	// from the contributor fan-out — a disabled model's role simply
	// doesn't participate in this consensus round, same as if it had never
	// been registered.
	now := time.Now()
	var primary *RegisteredModel
	var others []RegisteredModel
	for i := range models {
		if !models[i].DisabledUntil.IsZero() && now.Before(models[i].DisabledUntil) {
			continue
		}
		if models[i].IsPrimary {
			primary = &models[i]
		} else {
			others = append(others, models[i])
		}
	}
	// If no model is explicitly marked primary, use the first registered one.
	if primary == nil && len(models) > 0 {
		primary = &models[0]
	}
	if primary == nil || primary.Client == nil {
		return nil, fmt.Errorf("no primary model registered for consensus")
	}

	result := &core.ConsensusResult{}

	// If no other models are registered, fall back to primary-only.
	if len(others) == 0 {
		resp, err := r.callModel(ctx, primary.Client, primary.Name, messages,
			"You are the primary reasoning model. Provide a thorough, correct solution.")
		if err != nil {
			return nil, fmt.Errorf("primary model failed: %w", err)
		}
		result.Primary = resp
		result.Synthesized = resp
		if r.emitter != nil {
			r.emitter.EmitConsensus(map[string]interface{}{
				"message":     "primary only (no other models registered)",
				"model_count": 0,
				"conflict":    false,
				"primary":     primary.Name,
			}, false)
		}
		return result, nil
	}

	// --- Phase 5: Deterministic Pre-Routing & Selective Consensus ---
	taskType := r.classifier.Classify(goal)
	if taskType == TaskTypeDeterministic {
		// In a real system, the orchestrator would have bypassed the router.
		// If we reach here, we just return a fast primary answer.
		resp, err := r.callModel(ctx, primary.Client, primary.Name, messages, "Solve deterministically.")
		if err == nil {
			result.Primary = resp
			result.Synthesized = resp
			return result, nil
		}
	}

	allowedRoles := r.roleSelector.SelectRoles(taskType)
	roleSet := make(map[string]bool)
	for _, r := range allowedRoles {
		roleSet[r] = true
	}

	// Filter others by allowed roles
	var activeOthers []RegisteredModel
	for _, m := range others {
		if roleSet[m.Role] {
			activeOthers = append(activeOthers, m)
		}
	}

	// ── Phase 1: Fan out to primary + active non-primary models IN PARALLEL.
	type roleResult struct {
		idx    int
		name   string
		role   string
		output string
		weight float64
		err    error
	}

	allModels := append([]RegisteredModel{*primary}, activeOthers...)
	results := make([]roleResult, len(allModels))

	// runModel is the per-model fan-out body, shared by the parallel and serial
	// paths so they can never diverge. In Sequential mode it is called inline
	// (one model at a time → free-tier RPM-safe); in Parallel mode it is
	// dispatched on goroutines gated by a WaitGroup (today's behavior).
	runModel := func(idx int, model RegisteredModel) {
		weight := r.roleTracker.GetWeight(model.Role, model.Name)
		if model.Client == nil {
			results[idx] = roleResult{idx: idx, name: model.Name, role: model.Role, weight: weight, err: fmt.Errorf("no client")}
			return
		}
		prompt := roleSystemPrompt(model.Role)
		if r.emitter != nil {
			r.emitter.EmitTaskUpdate("consensus", "fan-out",
				fmt.Sprintf("%s (%s, w:%.2f) answering…", model.Name, model.Role, weight))
		}
		out, err := r.callModel(ctx, model.Client, model.Name, messages, prompt)
		results[idx] = roleResult{idx: idx, name: model.Name, role: model.Role, output: out, weight: weight, err: err}

		// Record success heuristically (if no error)
		r.roleTracker.RecordSuccess(model.Role, model.Name, err == nil)
	}

	r.mu.RLock()
	seq := r.sequentialConsensus
	r.mu.RUnlock()
	if seq {
		for i, m := range allModels {
			runModel(i, m)
		}
	} else {
		var wg sync.WaitGroup
		for i, m := range allModels {
			wg.Add(1)
			go func(idx int, model RegisteredModel) {
				defer wg.Done()
				runModel(idx, model)
			}(i, m)
		}
		wg.Wait()
	}

	// Build the synthesis prompt from all contributions, ordered by weight (simulated by just appending).
	// Ideally we would sort `results` slice by `weight` descending.
	// For simplicity, we just include the weight in the prompt for the LLM to see.
	var sb strings.Builder
	sb.WriteString("A user asked the following question. Multiple specialist models answered it from different perspectives. Your job is to synthesize their answers into a single, coherent, correct, and complete response.\n\n")
	sb.WriteString("Original question:\n")
	for _, m := range messages {
		if m.Role == core.RoleUser {
			sb.WriteString(fmt.Sprintf("%v\n", m.Content))
		}
	}
	sb.WriteString("\nSpecialist responses (higher weight = historically more reliable):\n\n")
	for _, rr := range results {
		role := rr.role
		if role == "" {
			role = "general"
		}
		if rr.err != nil {
			sb.WriteString(fmt.Sprintf("--- %s [%s] (weight: %.2f) [ERROR: %v] ---\n(skipped)\n\n", rr.name, role, rr.weight, rr.err))
		} else {
			sb.WriteString(fmt.Sprintf("--- %s [%s] (weight: %.2f) ---\n%s\n\n", rr.name, role, rr.weight, rr.output))
		}
	}
	sb.WriteString("\nSynthesize the above into a single, unified answer. Resolve any contradictions, heavily weight the most reliable specialists, and present the final answer clearly.")

	// Extract primary result for return
	for _, rr := range results {
		if rr.name == primary.Name && rr.err == nil {
			result.Primary = rr.output
		}
	}

	// callModel prepends its own systemPrompt argument to messages, so
	// synthMessages must stay system-message-free (matching the calling
	// convention used everywhere else in this file) — embedding a second
	// system message here previously caused every consensus synthesis call
	// to ship two stacked, overlapping system messages to the LLM.
	synthMessages := []core.Message{
		{Role: core.RoleUser, Content: sb.String()},
	}

	// ── Phase 2: Primary model synthesizes all contributions.
	synth, err := r.callModel(ctx, primary.Client, primary.Name, synthMessages,
		"You are the primary synthesizer. Merge the specialist responses from the "+
			"conversation below into a single, coherent, correct, and complete answer. "+
			"Resolve contradictions, incorporate the best insights from each specialist, "+
			"and present one unified, authoritative response.")
	if err != nil {
		// Fall back to the first successful contribution.
		for _, rr := range results {
			if rr.err == nil {
				result.Synthesized = rr.output
				break
			}
		}
		if result.Synthesized == "" {
			return nil, fmt.Errorf("primary synthesis failed and no contributions succeeded: %w", err)
		}
	} else {
		result.Synthesized = synth
	}

	// Populate contributions for the event/UI.
	for _, rr := range results {
		c := core.ModelContribution{Model: rr.name, Role: rr.role, Output: rr.output}
		if rr.err != nil {
			c.Error = rr.err.Error()
		}
		result.Contributions = append(result.Contributions, c)
	}

	// Detect conflict: check if any contribution strongly disagrees.
	result.Conflict = false
	for _, rr := range results {
		if rr.err == nil && detectConflict(result.Synthesized, rr.output) {
			result.Conflict = true
			break
		}
	}

	if r.emitter != nil {
		strategy := "parallel"
		if r.sequentialConsensus {
			strategy = "sequential"
		}
		r.emitter.EmitConsensus(map[string]interface{}{
			"message":     fmt.Sprintf("%d models fanned out, primary synthesized, conflict=%v", len(others), result.Conflict),
			"model_count": len(others),
			"conflict":    result.Conflict,
			"primary":     primary.Name,
			"strategy":    strategy,
			"elapsed_ms":  time.Since(consensusStart).Milliseconds(),
		}, result.Conflict)
	}

	return result, nil
}

// roleSystemPrompt returns the system prompt persona for a consensus role.
// If the role is empty or unrecognized, the "general" persona is used.
func roleSystemPrompt(role string) string {
	switch strings.ToLower(role) {
	case "critic":
		return "You are a critic. Answer the user's question, but focus on identifying potential flaws, edge cases, and limitations in the approach. Provide your answer while highlighting what could go wrong and how to guard against it."
	case "skeptic":
		return "You are a skeptic. Answer the user's question, but challenge the assumptions behind it. Question whether the question is asking the right thing, and provide your answer while noting what assumptions you are making and where they might not hold."
	case "knowledge_booster":
		return "You are a knowledge booster. Answer the user's question with extra depth: provide additional context, relevant facts, background knowledge, and references to related concepts that enrich the answer."
	case "creative":
		return "You are a creative thinker. Answer the user's question, but offer creative or unconventional approaches and alternatives. Think outside the box while remaining practical."
	case "analyst":
		return "You are an analyst. Answer the user's question by breaking it down analytically. Structure your answer with clear reasoning, pros/cons, and a logical breakdown of the problem."
	case "verifier":
		return "You are a verifier. Answer the user's question, then verify your own answer for correctness. Double-check facts, logic, and edge cases. Note any uncertainty."
	default:
		return "You are a specialist model. Answer the user's question thoroughly and correctly."
	}
}

// callModel sends a non-streaming completion request to a model.
func (r *Router) callModel(ctx context.Context, client core.LLMClient, model string, messages []core.Message, systemPrompt string) (string, error) {
	msgs := make([]core.Message, 0, len(messages)+1)
	msgs = append(msgs, core.Message{
		Role:    core.RoleSystem,
		Content: systemPrompt,
	})
	msgs = append(msgs, messages...)

	// Hard context-fit guarantee before dispatch (Part 3 contract): fit to
	// this consensus contributor's own effective window, so a persona model
	// on a smaller (e.g. local) tier never overflows.
	msgs = compression.FitClient(msgs, client, 0, 0)

	temp := 0.3
	maxTok := 4000
	req := &core.CompletionRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	resp, err := client.ChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty response from model")
	}

	return resp.Choices[0].Message.Content, nil
}

// detectConflict checks if the critique strongly disagrees with the primary output.
//
// It is a heuristic: it flags the critique as conflicting when it contains a
// strong disagreement marker ("incorrect", "wrong", "flaw", ...) UNLESS that
// marker appears in a negated context ("no error here", "not wrong",
// "error-free", "flawless"). This avoids the previous false-positive where a
// critique that explicitly says "there is no flaw" was itself treated as a
// conflict. The `primary` argument is retained for callers/signature stability
// and for future polarity comparisons against the synthesized output.
func detectConflict(primary, critique string) bool {
	if strings.TrimSpace(critique) == "" {
		return false
	}
	disagreementMarkers := []string{
		"incorrect", "wrong", "error", "flaw", "mistake",
		"disagree", "contradicts", "invalid", "false",
	}
	critiqueLower := strings.ToLower(critique)

	for _, marker := range disagreementMarkers {
		searchFrom := 0
		for {
			pos := strings.Index(critiqueLower[searchFrom:], marker)
			if pos < 0 {
				break
			}
			absPos := searchFrom + pos
			endPos := absPos + len(marker)

			// Suffix negation: "error-free", "flawless", "mistake-free", etc.
			after := critiqueLower[endPos:]
			if strings.HasPrefix(after, "-free") || strings.HasPrefix(after, "-less") ||
				strings.HasPrefix(after, " free") || strings.HasPrefix(after, "less") ||
				strings.HasPrefix(after, "-proof") {
				searchFrom = endPos
				continue
			}

			// Prefix negation: inspect up to 32 chars before the marker for a
			// negation word ("no", "not", "without", "isn't", "never", ...).
			winStart := absPos - 32
			if winStart < 0 {
				winStart = 0
			}
			window := critiqueLower[winStart:absPos]
			if !windowHasNegation(window) {
				return true
			}
			searchFrom = endPos
		}
	}
	return false
}

// windowHasNegation reports whether the given preceding-text window contains a
// negation term that would invert the meaning of a following marker.
func windowHasNegation(window string) bool {
	negations := []string{
		"no ", "not ", "without ", "free of ", "isn't ", "isnt ",
		"no real", "nothing ", "never ", "absence of ", "lack of ",
		"sans ", "doesn't ", "doesnt ", "aren't ", "arent ",
		"cannot ", "can't ", "no actual ", "no genuine ",
	}
	for _, n := range negations {
		if strings.Contains(window, n) {
			return true
		}
	}
	return false
}

// AssessComplexity estimates task complexity on a 0-10 scale based on
// the task description. This drives escalation decisions.
func AssessComplexity(taskDesc string) int {
	score := 3 // baseline

	complexityIndicators := []struct {
		pattern string
		weight  int
	}{
		{"architect", 3},
		{"design", 2},
		{"refactor", 2},
		{"debug", 2},
		{"optimize", 2},
		{"deploy", 2},
		{"multi", 1},
		{"parallel", 1},
		{"distributed", 2},
		{"concurrent", 1},
		{"security", 2},
		{"database", 1},
		{"migration", 2},
		{"integration", 1},
		{"test", 1},
		{"document", 1},
	}

	descLower := strings.ToLower(taskDesc)
	for _, indicator := range complexityIndicators {
		if strings.Contains(descLower, indicator.pattern) {
			score += indicator.weight
		}
	}

	// Length-based complexity
	if len(taskDesc) > 500 {
		score += 1
	}
	if len(taskDesc) > 1000 {
		score += 1
	}

	// Multiple sentences often indicate complex tasks
	if strings.Count(taskDesc, ".") > 3 {
		score += 1
	}

	if score > 10 {
		score = 10
	}
	return score
}
