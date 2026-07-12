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

// MCP (Model Context Protocol) support
// Implements the JSON-RPC 2.0 based protocol for tool discovery and execution.
// Spec: https://modelcontextprotocol.io/

// MCPRequest is a JSON-RPC 2.0 request.
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse is a JSON-RPC 2.0 response.
type MCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *MCPError       `json:"error,omitempty"`
}

// MCPError is a JSON-RPC 2.0 error.
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP tool definition (matches MCP spec shape).
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCP Server info.
type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCP capabilities.
type MCPCapabilities struct {
	Tools *struct{} `json:"tools,omitempty"`
}

// handleMCP handles MCP JSON-RPC requests over HTTP.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMCPError(w, nil, -32700, "Parse error")
		return
	}

	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	switch req.Method {
	case "initialize":
		s.mcpInitialize(w, req)
	case "tools/list":
		s.mcpToolsList(w, req)
	case "tools/call":
		s.mcpToolsCall(w, req)
	case "ping":
		writeMCPResult(w, req.ID, map[string]interface{}{"pong": true})
	default:
		writeMCPError(w, req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) mcpInitialize(w http.ResponseWriter, req MCPRequest) {
	writeMCPResult(w, req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"serverInfo": MCPServerInfo{
			Name:    "darkcode",
			Version: "1.0.0",
		},
		"capabilities": MCPCapabilities{
			Tools: &struct{}{},
		},
	})
}

func (s *Server) mcpToolsList(w http.ResponseWriter, req MCPRequest) {
	entries := s.registry.AllEntries()
	mcpTools := make([]MCPTool, len(entries))
	for i, te := range entries {
		// Convert ToolEntry parameters to MCP inputSchema
		inputSchema := map[string]interface{}{
			"type":       "object",
			"properties": te.Parameters,
		}
		mcpTools[i] = MCPTool{
			Name:        te.Name,
			Description: te.Description,
			InputSchema: inputSchema,
		}
	}
	writeMCPResult(w, req.ID, map[string]interface{}{
		"tools": mcpTools,
	})
}

func (s *Server) mcpToolsCall(w http.ResponseWriter, req MCPRequest) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeMCPError(w, req.ID, -32602, "Invalid params: "+err.Error())
		return
	}

	if params.Name == "" {
		writeMCPError(w, req.ID, -32602, "Tool name required")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, core.WorkspaceKey, s.ActiveWorkspace())

	result, err := s.registry.Execute(ctx, params.Name, params.Arguments)
	if err != nil {
		writeMCPError(w, req.ID, -32603, fmt.Sprintf("Tool execution failed: %v", err))
		return
	}

	// MCP returns content blocks
	var contentBlocks []map[string]interface{}
	contentBlocks = append(contentBlocks, map[string]interface{}{
		"type": "text",
		"text": result.Output,
	})

	writeMCPResult(w, req.ID, map[string]interface{}{
		"content": contentBlocks,
		"isError": !result.Success,
	})
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors are 200
	json.NewEncoder(w).Encode(MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    code,
			Message: msg,
		},
	})
}

// ensure tools import used
var _ tools.ToolResult
