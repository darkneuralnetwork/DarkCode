package router

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/darkcode/capability"
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
	r.mu.RUnlock()

	if client == nil {
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
	if r.advisor != nil && r.advisor.PreferLocal() && preferred != core.ModelTierReasoning && preferred != core.ModelTierCritic {
		for _, t := range []core.ModelTier{core.ModelTierLocal, core.ModelTierMediumLocal, core.ModelTierTinyLocal} {
			if _, ok := r.clients[t]; ok {
				return t
			}
		}
	}

	// Try preferred tier
	if _, ok := r.clients[preferred]; ok {
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
		if _, ok := r.clients[tier]; ok {
			return tier
		}
	}

	return preferred // will cause error if nothing registered
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
	models := make([]RegisteredModel, len(r.allModels))
	copy(models, r.allModels)
	r.mu.RUnlock()

	if mode != core.RouteConsensus {
		return nil, fmt.Errorf("not in consensus mode")
	}

	// Identify the primary model and the contributing (non-primary) models.
	var primary *RegisteredModel
	var others []RegisteredModel
	for i := range models {
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
		r.emitter.EmitConsensus(map[string]interface{}{
			"message":     fmt.Sprintf("%d models fanned out, primary synthesized, conflict=%v", len(others), result.Conflict),
			"model_count": len(others),
			"conflict":    result.Conflict,
			"primary":     primary.Name,
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
