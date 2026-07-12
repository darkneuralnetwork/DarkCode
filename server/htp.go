package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/tools"
)

// HTP — DarkCode Tool Protocol
//
// A custom JSON-based protocol designed specifically for the DarkCode agent
// operating system. Unlike MCP (which is JSON-RPC 2.0), HTP uses a simpler
// request/response envelope with built-in support for:
//
//   - Tool discovery with rich metadata (category, timeout hints, sandbox flags)
//   - Batch execution (multiple tools in one request, executed concurrently)
//   - Streaming results (optional SSE callback for long-running tools)
//   - Capability negotiation (client declares what it supports)
//   - Sandboxed execution with path restrictions
//   - Tool chaining (output of tool A feeds into tool B)
//
// Protocol version: 1.0
//
// Transport: HTTP POST (JSON body) or WebSocket (for streaming)
//
// Request envelope:
//   {
//     "htp_version": "1.0",
//     "action": "list|execute|batch|schema|health|chain",
//     "tool": "tool_name",          // for execute
//     "args": {...},                 // for execute
//     "tools": [...],                // for batch
//     "chain": [...],                // for chain
//     "client_capabilities": {...}   // optional
//   }
//
// Response envelope:
//   {
//     "htp_version": "1.0",
//     "ok": true|false,
//     "action": "...",
//     "result": {...},               // single result
//     "results": [...],              // batch results
//     "error": "...",                // if ok=false
//     "server_info": {...}           // always present
//   }

// HTPVersion is the protocol version.
const HTPVersion = "1.0"

// HTPRequest is the request envelope.
type HTPRequest struct {
	HTPVersion         string                 `json:"htp_version"`
	Action             string                 `json:"action"`
	Tool               string                 `json:"tool,omitempty"`
	Args               map[string]interface{} `json:"args,omitempty"`
	Tools              []HTPToolCall          `json:"tools,omitempty"` // batch
	Chain              []HTPChainStep         `json:"chain,omitempty"` // chain
	ClientCapabilities HTPClientCapabilities  `json:"client_capabilities,omitempty"`
}

// HTPToolCall is a single tool invocation in a batch.
type HTPToolCall struct {
	ID   string                 `json:"id"`
	Tool string                 `json:"tool"`
	Args map[string]interface{} `json:"args"`
}

// HTPChainStep is a step in a tool chain.
type HTPChainStep struct {
	Tool     string                 `json:"tool"`
	Args     map[string]interface{} `json:"args"`
	ArgsFrom string                 `json:"args_from,omitempty"` // JSON path from previous result
}

// HTPClientCapabilities declares what the client supports.
type HTPClientCapabilities struct {
	Streaming      bool     `json:"streaming"`
	Sandbox        bool     `json:"sandbox"`
	MaxParallel    int      `json:"max_parallel"`
	SupportedTypes []string `json:"supported_types"`
}

// HTPResponse is the response envelope.
type HTPResponse struct {
	HTPVersion string        `json:"htp_version"`
	OK         bool          `json:"ok"`
	Action     string        `json:"action"`
	Result     interface{}   `json:"result,omitempty"`
	Results    []interface{} `json:"results,omitempty"`
	Error      string        `json:"error,omitempty"`
	ServerInfo HTPServerInfo `json:"server_info"`
}

// HTPServerInfo describes the server.
type HTPServerInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Protocol    string `json:"protocol"`
	ToolCount   int    `json:"tool_count"`
	Sandboxed   bool   `json:"sandboxed"`
	MaxParallel int    `json:"max_parallel"`
}

// HTPToolInfo is rich tool metadata.
type HTPToolInfo struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Category    string                 `json:"category"`
	Parameters  map[string]interface{} `json:"parameters"`
	Sandboxed   bool                   `json:"sandboxed"`
	Timeout     int                    `json:"timeout_seconds"`
	Destructive bool                   `json:"destructive"`
}

// handleHTP handles DarkCode Tool Protocol requests.
func (s *Server) handleHTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxHTPBodyBytes)
	var req HTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTPError(w, "parse", "Parse error: "+err.Error())
		return
	}

	if req.HTPVersion == "" {
		req.HTPVersion = HTPVersion
	}

	switch req.Action {
	case "health":
		s.htpHealth(w, req)
	case "list":
		s.htpList(w, req)
	case "schema":
		s.htpSchema(w, req)
	case "execute":
		s.htpExecute(w, req)
	case "batch":
		s.htpBatch(w, req)
	case "chain":
		s.htpChain(w, req)
	default:
		writeHTPError(w, req.Action, fmt.Sprintf("Unknown action: %s", req.Action))
	}
}

func (s *Server) htpServerInfo() HTPServerInfo {
	s.cfgMu.RLock()
	safetyLevel, maxConcurrent := s.cfg.SafetyLevel, s.cfg.MaxConcurrent
	s.cfgMu.RUnlock()
	return HTPServerInfo{
		Name:        "darkcode",
		Version:     "1.0.0",
		Protocol:    HTPVersion,
		ToolCount:   len(s.registry.AllEntries()),
		Sandboxed:   safetyLevel == "strict",
		MaxParallel: maxConcurrent,
	}
}

func (s *Server) htpHealth(w http.ResponseWriter, req HTPRequest) {
	writeHTPResult(w, "health", map[string]interface{}{
		"status": "healthy",
		"tools":  len(s.registry.Schemas()),
		"uptime": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) htpList(w http.ResponseWriter, req HTPRequest) {
	s.cfgMu.RLock()
	safetyLevel := s.cfg.SafetyLevel
	s.cfgMu.RUnlock()
	entries := s.registry.AllEntries()
	toolsList := make([]HTPToolInfo, len(entries))
	for i, te := range entries {
		destructive := false
		sandboxed := false
		timeout := 30
		// Classify tools
		switch te.Name {
		case "terminal", "write_file", "patch":
			destructive = true
		}
		if te.Name == "terminal" {
			sandboxed = safetyLevel == "strict"
		}
		toolsList[i] = HTPToolInfo{
			Name:        te.Name,
			Description: te.Description,
			Category:    te.Category,
			Parameters:  toolParamsMap(te), // include the JSON-Schema so remote callers can register faithfully
			Sandboxed:   sandboxed,
			Timeout:     timeout,
			Destructive: destructive,
		}
	}
	writeHTPResult(w, "list", map[string]interface{}{
		"tools": toolsList,
		"count": len(toolsList),
	})
}

func (s *Server) htpSchema(w http.ResponseWriter, req HTPRequest) {
	if req.Tool == "" {
		writeHTPError(w, "schema", "tool name required")
		return
	}

	entries := s.registry.AllEntries()
	for _, te := range entries {
		if te.Name == req.Tool {
			writeHTPResult(w, "schema", HTPToolInfo{
				Name:        te.Name,
				Description: te.Description,
				Category:    te.Category,
				Timeout:     30,
			})
			return
		}
	}
	writeHTPError(w, "schema", fmt.Sprintf("Tool not found: %s", req.Tool))
}

func (s *Server) htpExecute(w http.ResponseWriter, req HTPRequest) {
	if req.Tool == "" {
		writeHTPError(w, "execute", "tool name required")
		return
	}

	timeout := 60
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	result, err := s.registry.Execute(ctx, req.Tool, req.Args)
	if err != nil {
		writeHTPError(w, "execute", err.Error())
		return
	}

	writeHTPResult(w, "execute", map[string]interface{}{
		"name":    result.Name,
		"success": result.Success,
		"output":  result.Output,
		"error":   result.Error,
	})
}

func (s *Server) htpBatch(w http.ResponseWriter, req HTPRequest) {
	if len(req.Tools) == 0 {
		writeHTPError(w, "batch", "tools array required")
		return
	}

	// Execute all tools concurrently
	type batchResult struct {
		ID     string      `json:"id"`
		Tool   string      `json:"tool"`
		Result interface{} `json:"result,omitempty"`
		Error  string      `json:"error,omitempty"`
		OK     bool        `json:"ok"`
	}

	results := make([]interface{}, len(req.Tools))
	type execResult struct {
		index  int
		id     string
		tool   string
		result *tools.ToolResult
		err    error
	}
	resultCh := make(chan execResult, len(req.Tools))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, core.WorkspaceKey, s.ActiveWorkspace())

	for i, tc := range req.Tools {
		go func(idx int, call HTPToolCall) {
			result, err := s.registry.Execute(ctx, call.Tool, call.Args)
			resultCh <- execResult{idx, call.ID, call.Tool, result, err}
		}(i, tc)
	}

	for i := 0; i < len(req.Tools); i++ {
		r := <-resultCh
		br := batchResult{
			ID:   r.id,
			Tool: r.tool,
			OK:   r.err == nil && (r.result != nil && r.result.Success),
		}
		if r.err != nil {
			br.Error = r.err.Error()
		} else if r.result != nil {
			br.Result = map[string]interface{}{
				"name":    r.result.Name,
				"success": r.result.Success,
				"output":  r.result.Output,
				"error":   r.result.Error,
			}
		}
		results[r.index] = br
	}

	writeHTPResult(w, "batch", map[string]interface{}{
		"results": results,
		"count":   len(results),
		"all_ok":  allBatchOK(results),
	})
}

func (s *Server) htpChain(w http.ResponseWriter, req HTPRequest) {
	if len(req.Chain) == 0 {
		writeHTPError(w, "chain", "chain steps required")
		return
	}

	// Execute tools sequentially, passing output forward
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	var chainResults []map[string]interface{}
	var lastOutput string

	for i, step := range req.Chain {
		args := make(map[string]interface{})
		for k, v := range step.Args {
			args[k] = v
		}
		// If args_from is specified, inject previous output
		if step.ArgsFrom != "" && lastOutput != "" {
			args[step.ArgsFrom] = lastOutput
		}

		result, err := s.registry.Execute(ctx, step.Tool, args)
		if err != nil {
			writeHTPError(w, "chain", fmt.Sprintf("Step %d (%s) failed: %v", i+1, step.Tool, err))
			return
		}

		if result.Output != "" {
			lastOutput = result.Output
		}

		chainResults = append(chainResults, map[string]interface{}{
			"step": i + 1,
			"tool": step.Tool,
			"result": map[string]interface{}{
				"name":    result.Name,
				"success": result.Success,
				"output":  result.Output,
				"error":   result.Error,
			},
		})
	}

	writeHTPResult(w, "chain", map[string]interface{}{
		"steps":        chainResults,
		"count":        len(chainResults),
		"final_output": lastOutput,
	})
}

func writeHTPResult(w http.ResponseWriter, action string, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HTPResponse{
		HTPVersion: HTPVersion,
		OK:         true,
		Action:     action,
		Result:     result,
		ServerInfo: HTPServerInfo{
			Name:     "darkcode",
			Version:  "1.0.0",
			Protocol: HTPVersion,
		},
	})
}

// toolParamsMap returns the JSON-Schema parameters of a tool entry as a
// map (for the HTP list response). Returns an empty object schema if the
// entry has none.
func toolParamsMap(te *tools.ToolEntry) map[string]interface{} {
	if te == nil || len(te.Parameters) == 0 {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(te.Parameters, &m); err != nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	return m
}

func writeHTPError(w http.ResponseWriter, action, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HTPResponse{
		HTPVersion: HTPVersion,
		OK:         false,
		Action:     action,
		Error:      msg,
		ServerInfo: HTPServerInfo{
			Name:     "darkcode",
			Version:  "1.0.0",
			Protocol: HTPVersion,
		},
	})
}

func allBatchOK(results []interface{}) bool {
	for _, r := range results {
		if m, ok := r.(map[string]interface{}); ok {
			if okVal, exists := m["ok"]; exists {
				if ok, isBool := okVal.(bool); isBool && !ok {
					return false
				}
			}
		}
	}
	return true
}

// ensure tools import used
var _ tools.ToolResult
