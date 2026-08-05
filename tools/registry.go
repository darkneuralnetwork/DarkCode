package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/checkpoint"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/permission"
	"github.com/darkcode/spill"
	"github.com/darkcode/ui"
)

// ToolResult is the output of executing a tool.
type ToolResult struct {
	Name    string `json:"name"`
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

// ToolHandler is the function signature for tool execution.
// It receives the parsed arguments as a map and a context for cancellation.
type ToolHandler func(ctx context.Context, args map[string]interface{}) *ToolResult

// ToolEntry describes a registered tool: its schema and handler.
type ToolEntry struct {
	Name          string
	Description   string
	Parameters    json.RawMessage // JSON Schema
	Handler       ToolHandler
	Category      string // toolset name
	Source        string // id of the tool source that registered this tool ("builtin" if built-in)
	Deterministic bool   // if true, router checks this before involving any LLM
	// ReadOnly marks a tool that only observes (reads files, lists, searches,
	// fetches) and never mutates the filesystem/system. Chat mode offers only
	// read-only tools so it can answer from the project without writing.
	ReadOnly bool

	// ReadOnlyWhen decides read-only-ness per CALL when a tool has both kinds
	// of operation. nil means the static ReadOnly flag decides.
	//
	// A single flag cannot describe `pdf`: info and extract_text observe,
	// while merge, split and rotate write new files. Flagged read-only it
	// would let a Chat turn write; flagged otherwise — which is what shipped —
	// a Chat turn cannot read a PDF at all, and the same was true of research
	// and graph_query. That is how a "conversation" mode ends up unable to
	// look anything up.
	//
	// The permission gate already classifies risk from the tool AND its
	// arguments. This is the same idea for the read-only boundary.
	ReadOnlyWhen func(args map[string]interface{}) bool
}

// Registry holds all registered tools. Thread-safe.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]*ToolEntry
	gate     *permission.Gate    // optional permission gate
	recorder *ChangeRecorder     // optional change recorder
	emitter  *ui.EventEmitter    // optional event emitter for file_change events
	breaker  *toolBreaker        // per-tool circuit breaker (self-healing runtime)
	ckpt     *checkpoint.Manager // optional pre-mutation snapshotter
	// spill offloads oversized tool results to disk and leaves a preview plus a
	// retrievable handle in the context. nil = fall back to truncation. The
	// registry owns this because it owns tool execution: what a tool result
	// looks like once it reaches the model is a property of running the tool,
	// not of whichever loop happened to call it.
	spill *spill.Store
	// observeFile records what the agent has seen of a file so the knowledge
	// graph knows which of its beliefs are about a version that no longer
	// exists. nil = not recording. See memory.System.ObserveFile.
	observeFile func(path, content string)
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]*ToolEntry),
		breaker: newToolBreaker(),
	}
}

// Register adds a tool to the registry. Panics if the name is already taken
// (this catches duplicate registration at init time).
func (r *Registry) Register(entry *ToolEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[entry.Name]; exists {
		log.Printf("WARNING: tool %q already registered, overwriting", entry.Name)
	}
	r.tools[entry.Name] = entry
}

// Unregister removes a tool from the registry by name. It is used when a
// tool source (MCP server, in-house tool file) is disconnected at runtime so
// its tools are no longer callable by the agent. Returns true if a tool was
// removed. Safe to call with a non-existent name (returns false).
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	return true
}

// UnregisterBySource removes every tool that was registered by the given
// source id. Returns the list of tool names that were removed. Used when a
// whole tool source is disconnected.
func (r *Registry) UnregisterBySource(sourceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	for name, entry := range r.tools {
		if entry.Source == sourceID {
			delete(r.tools, name)
			removed = append(removed, name)
		}
	}
	return removed
}

// ListBySource returns the names of all tools registered by a given source.
func (r *Registry) ListBySource(sourceID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name, entry := range r.tools {
		if entry.Source == sourceID {
			names = append(names, name)
		}
	}
	return names
}

// Get retrieves a tool by name.
func (r *Registry) Get(name string) (*ToolEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	return entry, ok
}

// List returns all registered tool entries.
func (r *Registry) List() []*ToolEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*ToolEntry, 0, len(r.tools))
	for _, entry := range r.tools {
		result = append(result, entry)
	}
	return result
}

// Schemas returns the OpenAI-compatible tool definitions for all
// registered tools, suitable for the `tools` field of a chat completion
// request.
func (r *Registry) Schemas() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ToolDef, 0, len(r.tools))
	for _, entry := range r.tools {
		result = append(result, ToolDef{
			Type: "function",
			Function: FunctionDef{
				Name:        entry.Name,
				Description: entry.Description,
				Parameters:  entry.Parameters,
			},
		})
	}
	return result
}

// LLMSchemas returns the registered tools as llm.ToolSchema values, ready to
// pass directly as the `Tools` field of an llm.CompletionRequest. This
// centralizes the tools.ToolDef → llm.ToolSchema mapping that was previously
// duplicated (identically) in the agent, agents, and loop packages.
//
// (tools → llm is acyclic: llm only depends on config/core/metrics, none of
// which depend on tools. The earlier “avoid import cycles” comment predating
// this was overly conservative — verified by `go build`.)
func (r *Registry) LLMSchemas() interface{} {
	return r.llmSchemas(false)
}

// LLMSchemasReadOnly returns only the read-only tool schemas — used by Chat
// mode so the model is never even offered a write/execute tool.
func (r *Registry) LLMSchemasReadOnly() interface{} {
	return r.llmSchemas(true)
}

func (r *Registry) llmSchemas(readOnlyOnly bool) []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]llm.ToolSchema, 0, len(r.tools))
	for _, entry := range r.tools {
		if readOnlyOnly && !entry.ReadOnly {
			continue
		}
		result = append(result, llm.ToolSchema{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        entry.Name,
				Description: entry.Description,
				Parameters:  entry.Parameters,
			},
		})
	}
	return result
}

// IsReadOnly reports whether the named tool is read-only (observes, never
// mutates). Unknown tools are treated as NOT read-only (fail closed).
func (r *Registry) IsReadOnly(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.tools[name]; ok {
		return e.ReadOnly
	}
	return false
}

// readOnlyCall reports whether THIS invocation only observes. Falls back to
// the static flag, so a tool without ReadOnlyWhen behaves exactly as before —
// and a nil entry is not read-only, because the safe default for "I cannot
// tell" is to refuse in a read-only request.
func (e *ToolEntry) readOnlyCall(args map[string]interface{}) bool {
	if e == nil {
		return false
	}
	if e.ReadOnlyWhen != nil {
		return e.ReadOnlyWhen(args)
	}
	return e.ReadOnly
}

// ToolDef / FunctionDef mirror the llm.ToolSchema but live here so the
// registry package doesn't depend on the llm package (avoids import cycles).
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// DispatchResult holds the outcome of executing one tool call.
type DispatchResult struct {
	CallID string
	Name   string
	Result *ToolResult
}

// DispatchAll executes multiple tool calls concurrently using goroutines.
// Each tool call runs in its own goroutine with a per-call timeout.
// Results are collected and returned in the same order as the input calls.
//
// This is the core concurrency feature: when the LLM emits multiple tool
// calls in a single response, they all execute in parallel rather than
// sequentially, dramatically reducing wall-clock latency for multi-tool
// turns (e.g., "read 3 files and search the codebase" runs all 4 at once).
func (r *Registry) DispatchAll(ctx context.Context, calls []core.ToolCall) interface{} {
	if len(calls) == 0 {
		return nil
	}

	// One pre-mutation checkpoint for the whole batch: the calls run
	// concurrently, so snapshotting per call would both serialize them and
	// record several near-identical states of the same turn.
	r.snapshot(mutatingTool(r, calls))

	results := make([]DispatchResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, tc core.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release
			// Defense-in-depth: a panic inside a single tool must fail only
			// that tool call, never crash the whole server process (a tool
			// runs in its own goroutine, so an unrecovered panic would take
			// the process down and drop every in-flight request — surfacing
			// in the browser as a bare "NetworkError").
			defer func() {
				if rec := recover(); rec != nil {
					results[idx] = DispatchResult{
						CallID: tc.ID,
						Name:   tc.Function.Name,
						Result: &ToolResult{
							Name:    tc.Function.Name,
							Success: false,
							Error:   fmt.Sprintf("tool panicked: %v", rec),
						},
					}
				}
			}()

			results[idx] = r.dispatchOne(ctx, tc)
		}(i, call)
	}

	wg.Wait()
	return results
}

// defaultToolTimeout bounds a single tool handler's execution. Shared by every
// dispatch surface so they agree.
const defaultToolTimeout = 120 * time.Second

// mutatingTool returns the name of the first call that can modify the
// workspace, or "" when every call only observes.
func mutatingTool(r *Registry, calls []core.ToolCall) string {
	for _, c := range calls {
		if entry, ok := r.Get(c.Function.Name); ok && !entry.ReadOnly {
			return c.Function.Name
		}
	}
	return ""
}

// snapshot records a checkpoint before a mutating tool runs, so the user can
// undo it. A read-only turn (empty tool) or an unconfigured checkpointer is a
// no-op, and a snapshot failure must never block the tool — the user loses the
// undo point, not the action.
func (r *Registry) snapshot(tool string) {
	if tool == "" {
		return
	}
	r.mu.RLock()
	ckpt := r.ckpt
	r.mu.RUnlock()
	if ckpt == nil {
		return
	}
	if _, err := ckpt.Snapshot(tool, "before "+tool); err != nil {
		log.Printf("checkpoint: snapshot before %s failed: %v", tool, err)
	}
}

// readOnlyDeny returns a blocked result when a read-only request targets a
// mutating tool, else nil. Defense-in-depth behind not offering write tools.
//
// The reason travels with the context because there are two unrelated ones and
// the advice differs: a user in Chat mode can switch to Build, while a research
// sub-agent cannot switch anything and would waste turns looking for the
// control. Telling it the truth — that its role has no write authority — is
// what makes it stop trying.
func readOnlyDeny(ctx context.Context, name string, entry *ToolEntry, args map[string]interface{}) *ToolResult {
	if IsReadOnlyContext(ctx) && !entry.readOnlyCall(args) {
		reason := core.ReadOnlyReason(ctx)
		if reason == "" {
			reason = "this is a read-only (Chat) request — switch to Build mode to modify files"
		}
		return &ToolResult{Name: name, Success: false,
			Error: "blocked: " + name + " is a write/execute tool and " + reason}
	}
	return nil
}

// gateDeny consults the permission gate and returns a denied result (carrying
// any user feedback) when the call is refused, else nil. Shared so every
// dispatch surface — ReAct/DAG and the direct /api/tools/execute + /api/htp
// path — gates identically.
func (r *Registry) gateDeny(name string, args map[string]interface{}) *ToolResult {
	r.mu.RLock()
	gate := r.gate
	r.mu.RUnlock()
	if gate == nil {
		return nil
	}
	allowed, req, feedback := gate.Check(name, args)
	if allowed {
		return nil
	}
	msg := "permission denied by user" + denySuffix(req)
	if strings.TrimSpace(feedback) != "" {
		msg += "\nUser feedback: " + strings.TrimSpace(feedback)
	}
	return &ToolResult{Name: name, Success: false, Error: msg}
}

// dispatchOne executes a single tool call with a timeout.
// It enforces the permission gate (if installed) and records before/after
// state for mutating tools (file writes, patches, shell commands, git ops).
func (r *Registry) dispatchOne(ctx context.Context, call core.ToolCall) DispatchResult {
	result := DispatchResult{
		CallID: call.ID,
		Name:   call.Function.Name,
	}

	entry, ok := r.Get(call.Function.Name)
	if !ok {
		result.Result = &ToolResult{
			Name:    call.Function.Name,
			Success: false,
			Error:   fmt.Sprintf("unknown tool: %s", call.Function.Name),
		}
		return result
	}

	// Parse arguments JSON
	var args map[string]interface{}
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			result.Result = &ToolResult{
				Name:    call.Function.Name,
				Success: false,
				Error:   fmt.Sprintf("invalid arguments JSON: %v", err),
			}
			return result
		}
	} else {
		args = make(map[string]interface{})
	}

	if msg := validateArgs(entry.Parameters, args); msg != "" {
		result.Result = &ToolResult{
			Name:    call.Function.Name,
			Success: false,
			Error:   "invalid arguments: " + msg,
		}
		return result
	}

	// Read-only policy + permission gate (shared with the direct-execute path).
	if blocked := readOnlyDeny(ctx, call.Function.Name, entry, args); blocked != nil {
		result.Result = blocked
		return result
	}
	if denied := r.gateDeny(call.Function.Name, args); denied != nil {
		result.Result = denied
		return result
	}

	// Self-healing runtime: if this tool has been failing repeatedly it is
	// quarantined — short-circuit instead of running it again. The message
	// doubles as a steer for the LLM to try a different approach. Reached only
	// after validation + permission pass, so those never trip the breaker.
	if ok, remaining := r.breaker.allow(call.Function.Name); !ok {
		result.Result = &ToolResult{
			Name:    call.Function.Name,
			Success: false,
			Error: fmt.Sprintf("tool %q is temporarily unavailable (quarantined after repeated failures; retry in ~%s, or use a different approach)",
				call.Function.Name, remaining.Round(time.Second)),
		}
		return result
	}

	// Capture the "before" state for file-mutating tools so we can show a
	// before→after diff afterwards.
	beforePath, beforeContent, beforeExists := captureFileBefore(ctx, call.Function.Name, args)

	// Per-tool timeout (120s default; can be overridden via context)
	toolCtx, cancel := context.WithTimeout(ctx, defaultToolTimeout)
	defer cancel()

	started := time.Now()
	res := entry.Handler(toolCtx, args)
	if res == nil {
		res = &ToolResult{Name: call.Function.Name, Success: false, Error: "tool returned nil result"}
	}
	if res.Name == "" {
		res.Name = call.Function.Name
	}
	result.Result = res

	// Feed the outcome to the circuit breaker: a genuine execution failure
	// counts toward quarantine; any success fully clears the tool's failure
	// state (recovery).
	if res.Success {
		r.breaker.recordSuccess(call.Function.Name)
	} else {
		r.breaker.recordFailure(call.Function.Name)
	}

	// Record what changed (files, commands, git ops) for the activity log
	// and the inline after-query summary.
	r.recordChange(ctx, call.Function.Name, args, res, beforePath, beforeContent, beforeExists, started)
	r.noteFileObservation(ctx, call.Function.Name, args, res)

	return result
}

// denySuffix renders a short hint about what was denied.
func denySuffix(req permission.ApprovalRequest) string {
	if req.Summary == "" {
		return ""
	}
	return " (" + req.Summary + ")"
}

// captureFileBefore reads the current content of the file that a file-mutating
// tool is about to touch. Returns (path, content, existed).
func captureFileBefore(ctx context.Context, tool string, args map[string]interface{}) (string, string, bool) {
	if tool != "write_file" && tool != "patch" {
		return "", "", false
	}
	path, _ := args["path"].(string)
	if path == "" {
		return "", "", false
	}
	path = expandPath(ctx, path)
	data, err := os.ReadFile(path)
	if err != nil {
		return path, "", false
	}
	return path, string(data), true
}

// recordChange builds and stores a Change record for mutating tools and
// emits a file_change event when an emitter is installed.
func (r *Registry) recordChange(ctx context.Context, tool string, args map[string]interface{}, res *ToolResult, beforePath, beforeContent string, beforeExists bool, started time.Time) {
	r.mu.RLock()
	rec := r.recorder
	em := r.emitter
	r.mu.RUnlock()
	if rec == nil && em == nil {
		return
	}

	var c core.Change
	c.Tool = tool
	c.Success = res.Success
	c.Timestamp = started
	c.Output = res.Output

	switch tool {
	case "write_file":
		path := beforePath
		if path == "" {
			path = expandPath(ctx, str(args["path"]))
		}
		after, _ := os.ReadFile(path)
		c.Kind = core.ChangeFileModify
		if !beforeExists {
			c.Kind = core.ChangeFileCreate
		}
		c.Path = path
		c.Before = beforeContent
		c.After = string(after)
	case "patch":
		path := beforePath
		if path == "" {
			path = expandPath(ctx, str(args["path"]))
		}
		after, _ := os.ReadFile(path)
		c.Kind = core.ChangeFileModify
		c.Path = path
		c.Before = beforeContent
		c.After = string(after)
	case "terminal":
		c.Kind = core.ChangeCommand
		c.Command = str(args["command"])
		c.Output = res.Output
		if !res.Success {
			c.ExitCode = 1
		}
	case "git":
		action := str(args["action"])
		if permission.IsGitMutating(action) {
			c.Kind = core.ChangeGit
			c.Command = "git " + action + " " + str(args["args"])
			c.Output = res.Output
		}
	default:
		// Non-mutating tools are not recorded.
		return
	}

	if rec != nil {
		rec.Record(c)
	}
	if em != nil {
		em.Emit(core.EventFileChange, c, ui.WithTool(tool), ui.WithStatus(string(c.Kind)))
	}
}

// str is a small helper that coerces an interface{} to a string.
func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

// MustParseSchema is a helper that panics on invalid JSON schema.
// Used at tool registration time to catch errors early.
func MustParseSchema(schema string) json.RawMessage {
	// Validate it's valid JSON
	var v interface{}
	if err := json.Unmarshal([]byte(schema), &v); err != nil {
		panic(fmt.Sprintf("invalid tool schema JSON for: %s", schema))
	}
	return json.RawMessage(schema)
}

// jsonSchemaProp is the subset of JSON Schema's property object that
// argSchema checks: the declared type and (for strings) an optional enum.
type jsonSchemaProp struct {
	Type string   `json:"type"`
	Enum []string `json:"enum"`
}

// argSchema is the subset of JSON Schema that ToolEntry.Parameters uses
// across the codebase: a flat object with typed properties and a required
// list (see e.g. tools/memory_tool.go's Schema()).
type argSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]jsonSchemaProp `json:"properties"`
	Required   []string                  `json:"required"`
}

// validateArgs checks LLM-supplied tool arguments against the tool's own
// declared JSON Schema before dispatch: required fields must be present, and
// present fields must match their declared type (and enum, if any). This is
// intentionally a lightweight structural check (no $ref, nested schemas,
// numeric bounds, or pattern matching) rather than a full JSON Schema
// validator — the project has zero non-stdlib dependencies by design, and
// every tool schema in this codebase is a flat object. Returns "" if args
// are valid, or a human-readable error describing the first problem found.
func validateArgs(schema json.RawMessage, args map[string]interface{}) string {
	if len(schema) == 0 {
		return ""
	}
	var s argSchema
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return "" // schema doesn't follow the flat-object shape we check; skip
	}

	for _, req := range s.Required {
		if _, ok := args[req]; !ok {
			return fmt.Sprintf("missing required argument %q", req)
		}
	}

	for name, val := range args {
		prop, ok := s.Properties[name]
		if !ok || prop.Type == "" {
			continue // unknown-to-schema or untyped property; nothing to check
		}
		if !jsonTypeMatches(prop.Type, val) {
			return fmt.Sprintf("argument %q: expected type %s, got %s", name, prop.Type, jsonTypeName(val))
		}
		if len(prop.Enum) > 0 {
			if s, ok := val.(string); ok {
				valid := false
				for _, e := range prop.Enum {
					if s == e {
						valid = true
						break
					}
				}
				if !valid {
					return fmt.Sprintf("argument %q: %q is not one of %v", name, s, prop.Enum)
				}
			}
		}
	}
	return ""
}

// jsonTypeMatches reports whether a value decoded from JSON (via
// encoding/json into interface{}) matches a JSON Schema primitive type name.
func jsonTypeMatches(schemaType string, val interface{}) bool {
	switch schemaType {
	case "string":
		_, ok := val.(string)
		return ok
	case "number":
		_, ok := val.(float64)
		return ok
	case "integer":
		f, ok := val.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "array":
		_, ok := val.([]interface{})
		return ok
	case "object":
		_, ok := val.(map[string]interface{})
		return ok
	default:
		return true // unrecognized schema type — don't block on it
	}
}

// jsonTypeName describes the runtime type of a decoded JSON value for error
// messages.
func jsonTypeName(val interface{}) string {
	switch val.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", val)
	}
}

// Execute runs a single tool by name with the given arguments.
// This is a convenience method for direct tool execution (e.g., from the HTTP API).
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (*ToolResult, error) {
	entry, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}

	if entry.Handler == nil {
		return nil, fmt.Errorf("tool %s has no handler", name)
	}

	if msg := validateArgs(entry.Parameters, args); msg != "" {
		return nil, fmt.Errorf("invalid arguments for tool %s: %s", name, msg)
	}

	// Read-only policy + permission gate (shared with the ReAct/DAG dispatch
	// path) — the direct-execute surface must gate identically.
	if blocked := readOnlyDeny(ctx, name, entry, args); blocked != nil {
		return blocked, nil
	}
	if denied := r.gateDeny(name, args); denied != nil {
		return denied, nil
	}

	// Self-healing runtime: honor the circuit breaker here too, so the HTTP/
	// HTP tool-execution surface can't hammer a quarantined tool.
	if ok, remaining := r.breaker.allow(name); !ok {
		return nil, fmt.Errorf("tool %s is temporarily unavailable (quarantined after repeated failures; retry in ~%s)", name, remaining.Round(time.Second))
	}

	if !entry.ReadOnly {
		r.snapshot(name)
	}

	toolCtx, cancel := context.WithTimeout(ctx, defaultToolTimeout)
	defer cancel()

	result := entry.Handler(toolCtx, args)
	if result == nil {
		r.breaker.recordFailure(name)
		return nil, fmt.Errorf("tool %s returned nil result", name)
	}
	if result.Name == "" {
		result.Name = name
	}
	if result.Success {
		r.breaker.recordSuccess(name)
	} else {
		r.breaker.recordFailure(name)
	}
	return result, nil
}

// Category returns the category/toolset name for a tool.
func (r *Registry) Category(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.tools[name]; ok {
		return entry.Category
	}
	return ""
}

// SetPermissionGate installs a permission gate. When set, every tool call is
// checked by the gate before execution; dangerous actions require approval.
func (r *Registry) SetPermissionGate(gate *permission.Gate) {
	r.mu.Lock()
	r.gate = gate
	r.mu.Unlock()
}

// SetChangeRecorder installs a change recorder that captures before/after
// state for mutating tools (file writes, patches, shell commands, git ops).
func (r *Registry) SetChangeRecorder(rec *ChangeRecorder) {
	r.mu.Lock()
	r.recorder = rec
	r.mu.Unlock()
}

// SetCheckpointer installs the manager that snapshots the workspace before a
// mutating tool runs, backing /rollback.
func (r *Registry) SetCheckpointer(m *checkpoint.Manager) {
	r.mu.Lock()
	r.ckpt = m
	r.mu.Unlock()
}

// SetFileObserver installs the hook that records file contents the agent has
// seen. Called for reads AND writes: an agent that edits a file and does not
// update what it believes about it is wrong about the thing it just changed.
func (r *Registry) SetFileObserver(fn func(path, content string)) {
	r.mu.Lock()
	r.observeFile = fn
	r.mu.Unlock()
}

// noteFileObservation reports a file's current contents to the observer. It
// runs after a successful file tool call, reading from disk rather than from
// the tool's output, because read_file returns numbered lines and write_file
// returns a byte count — neither is the file.
func (r *Registry) noteFileObservation(ctx context.Context, tool string, args map[string]interface{}, res *ToolResult) {
	switch tool {
	case "read_file", "write_file", "patch", "replace_file_content":
	default:
		return
	}
	if res == nil || !res.Success {
		return
	}
	r.mu.RLock()
	observe := r.observeFile
	r.mu.RUnlock()
	if observe == nil {
		return
	}
	path, _ := args["path"].(string)
	if path == "" {
		return
	}
	path = expandPath(ctx, path)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	observe(path, string(data))
}

// SetSpillStore installs the store used to offload oversized tool results.
// Without one, results are truncated and the overflow is lost — see the spill
// package for why that was the largest avoidable source of both token waste
// and lost information in the agent.
func (r *Registry) SetSpillStore(s *spill.Store) {
	r.mu.Lock()
	r.spill = s
	r.mu.Unlock()
}

// ObserveResult returns the text for a tool result as it should appear in the
// model's context: unchanged when small, and a head/tail preview carrying a
// read_result handle when large.
//
// Every path that turns a tool result into a message goes through here, so the
// ReAct loop and a sub-agent cannot disagree about how much of a result the
// model gets to see.
func (r *Registry) ObserveResult(tool, output string) string {
	r.mu.RLock()
	st := r.spill
	r.mu.RUnlock()
	return spill.Observe(st, tool, output, spill.DefaultThreshold)
}

// SetEventEmitter installs an emitter used to broadcast file_change events.
func (r *Registry) SetEventEmitter(em *ui.EventEmitter) {
	r.mu.Lock()
	emitter := em
	r.emitter = emitter
	r.mu.Unlock()
}

// AllEntries returns all registered tool entries (for metadata queries).
func (r *Registry) AllEntries() []*ToolEntry {
	return r.List()
}

// readOnlyOperations builds a ReadOnlyWhen for tools that dispatch on a named
// sub-command, where some sub-commands observe and others write. It reads
// "operation" or "action", the two keys the built-in tools use.
//
// It answers false for an unrecognised or missing operation, so a new write
// operation added later is refused in a read-only request until someone
// deliberately lists it — the safe direction for a boundary nobody will
// remember to revisit.
func readOnlyOperations(observing ...string) func(map[string]interface{}) bool {
	allowed := make(map[string]bool, len(observing))
	for _, op := range observing {
		allowed[op] = true
	}
	return func(args map[string]interface{}) bool {
		op, _ := args["operation"].(string)
		if op == "" {
			op, _ = args["action"].(string)
		}
		return allowed[op]
	}
}
