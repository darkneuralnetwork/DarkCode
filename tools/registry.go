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

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/permission"
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
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	Handler     ToolHandler
	Category      string // toolset name
	Source        string // id of the tool source that registered this tool ("builtin" if built-in)
	Deterministic bool   // if true, router checks this before involving any LLM
}

// Registry holds all registered tools. Thread-safe.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]*ToolEntry
	gate     *permission.Gate // optional permission gate
	recorder *ChangeRecorder  // optional change recorder
	emitter  *ui.EventEmitter // optional event emitter for file_change events
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*ToolEntry)}
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]llm.ToolSchema, 0, len(r.tools))
	for _, entry := range r.tools {
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

	results := make([]DispatchResult, len(calls))
	var wg sync.WaitGroup
	// Fixed: Bounded worker pool (max 5 concurrent tool executions) to prevent resource exhaustion
	sem := make(chan struct{}, 5)

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, tc core.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			results[idx] = r.dispatchOne(ctx, tc)
		}(i, call)
	}

	wg.Wait()
	return results
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

	// Permission gate: ask before executing dangerous actions.
	r.mu.RLock()
	gate := r.gate
	r.mu.RUnlock()
	if gate != nil {
		allowed, req, feedback := gate.Check(call.Function.Name, args)
		if !allowed {
			msg := "permission denied by user" + denySuffix(req)
			// Surface the user's mid-execution feedback (if any) in the tool
			// result so the LLM sees the steer and adapts — e.g. "use /tmp
			// instead of /var". This works for both the ReAct loop and the
			// DAG worker path, which both feed tool results back to the model.
			if strings.TrimSpace(feedback) != "" {
				msg += "\nUser feedback: " + strings.TrimSpace(feedback)
			}
			result.Result = &ToolResult{
				Name:    call.Function.Name,
				Success: false,
				Error:   msg,
			}
			return result
		}
	}

	// Capture the "before" state for file-mutating tools so we can show a
	// before→after diff afterwards.
	beforePath, beforeContent, beforeExists := captureFileBefore(ctx, call.Function.Name, args)

	// Per-tool timeout (120s default; can be overridden via context)
	toolCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
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

	// Record what changed (files, commands, git ops) for the activity log
	// and the inline after-query summary.
	r.recordChange(ctx, call.Function.Name, args, res, beforePath, beforeContent, beforeExists, started)

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

	toolCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	result := entry.Handler(toolCtx, args)
	if result == nil {
		return nil, fmt.Errorf("tool %s returned nil result", name)
	}
	if result.Name == "" {
		result.Name = name
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
