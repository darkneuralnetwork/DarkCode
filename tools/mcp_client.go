package tools

// ============================================================================
// MCP CLIENT — connects DarkCode to EXTERNAL MCP servers as a client.
//
// This is the inverse of server/mcp.go (which exposes DarkCode's own tools
// as an MCP server). Here we dial out to an MCP server, run the standard
// `initialize` → `tools/list` → `tools/call` flow, and surface the remote
// tools as ordinary ToolEntry handlers in the local registry. From the
// agent's point of view, an MCP-backed tool is indistinguishable from a
// built-in one.
//
// Two transports are supported, matching the MCP spec:
//
//   • stdio — spawn a child process and speak JSON-RPC 2.0 over its
//            stdin/stdout (newline-delimited). This is how most local MCP
//            servers are run (e.g. npx -y @modelcontextprotocol/server-...).
//   • http  — POST JSON-RPC 2.0 envelopes to a URL endpoint.
//
// Each client implementation satisfies the MCPClient interface so the
// SourceManager can treat them uniformly.
// ============================================================================

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MCPClient is the contract for talking to an MCP server. The SourceManager
// uses it to discover and invoke remote tools.
type MCPClient interface {
	// Initialize performs the MCP handshake and returns the server info.
	Initialize(ctx context.Context) (MCPServerInfo, error)
	// ListTools returns the tool catalogue exposed by the server.
	ListTools(ctx context.Context) ([]MCPRemoteTool, error)
	// CallTool invokes a tool by name with JSON arguments and returns the
	// assembled text content.
	CallTool(ctx context.Context, name string, args map[string]interface{}) (string, bool, error)
	// Close releases any underlying process or connection.
	Close() error
}

// MCPServerInfo is the identity returned by an MCP `initialize` response.
type MCPServerInfo struct {
	Name            string                 `json:"name"`
	Version         string                 `json:"version"`
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
}

// MCPRemoteTool is a tool entry returned by `tools/list`.
type MCPRemoteTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ----------------------------------------------------------------------------
// JSON-RPC 2.0 envelopes
// ----------------------------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ----------------------------------------------------------------------------
// Stdio transport
// ----------------------------------------------------------------------------

// StdioMCPClient speaks MCP over a child process's stdin/stdout.
type StdioMCPClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu     sync.Mutex // serializes request/response cycles
	nextID int64
	lines  chan string // raw newline-delimited messages from stdout
	closed chan struct{}
	once   sync.Once
}

// NewStdioMCPClient spawns an MCP server process. The command is run with
// `args` and the given environment (nil inherits the parent env).
func NewStdioMCPClient(command string, args []string, env map[string]string) (*StdioMCPClient, error) {
	if command == "" {
		return nil, fmt.Errorf("mcp: command is required for stdio transport")
	}
	cmd := exec.Command(command, args...)
	if env != nil {
		// Start from the parent env so PATH etc. are preserved, then apply overrides.
		cmd.Env = append(cmd.Env, envToList(env)...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("mcp: start %s: %w", command, err)
	}

	c := &StdioMCPClient{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		lines:  make(chan string, 256),
		closed: make(chan struct{}),
	}
	// Background reader: pumps newline-delimited messages from the server
	// into the lines channel. Notifications (no id) and stray messages are
	// simply forwarded; the request loop ignores anything whose id doesn't
	// match the in-flight request.
	go c.readLoop()
	// Reap the process when it exits so we don't accumulate zombies.
	go func() {
		_ = cmd.Wait()
		close(c.closed)
	}()
	return c, nil
}

func envToList(env map[string]string) []string {
	base := os.Environ()
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	return base
}

// readLoop reads newline-delimited JSON messages from stdout.
func (c *StdioMCPClient) readLoop() {
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow large messages
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		select {
		case c.lines <- line:
		case <-c.closed:
			return
		default:
			// Buffer full: drop oldest-ish message to avoid blocking the reader.
			select {
			case <-c.lines:
			default:
			}
			select {
			case c.lines <- line:
			case <-c.closed:
				return
			}
		}
	}
}

// request sends a JSON-RPC request and waits for the matching response,
// discarding any notifications that arrive in the meantime.
func (c *StdioMCPClient) request(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := atomic.AddInt64(&c.nextID, 1)
	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("mcp: write: %w", err)
	}

	// Wait for the matching response (or timeout / context cancel).
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, fmt.Errorf("mcp: server process exited")
		case line := <-c.lines:
			var resp jsonRPCResponse
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue // not valid JSON-RPC; skip
			}
			if resp.ID == 0 {
				// Notification (no id) — ignore for now.
				continue
			}
			if resp.ID != id {
				// Out-of-order / stale response — skip.
				continue
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("mcp: %s: [%d] %s", method, resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

// Initialize performs the MCP handshake.
func (c *StdioMCPClient) Initialize(ctx context.Context) (MCPServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.request(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "darkcode",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return MCPServerInfo{}, err
	}
	// Send the initialized notification (no response expected).
	notif, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	_ = c.writeNotif(append(notif, '\n'))

	var info struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		ServerInfo      MCPServerInfo          `json:"serverInfo"`
		Capabilities    map[string]interface{} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return MCPServerInfo{}, fmt.Errorf("mcp: parse initialize: %w", err)
	}
	info.ServerInfo.ProtocolVersion = info.ProtocolVersion
	info.ServerInfo.Capabilities = info.Capabilities
	return info.ServerInfo, nil
}

// writeNotif writes a notification without taking the request lock for the
// response-wait path. It still serializes the stdin write against concurrent
// requests.
func (c *StdioMCPClient) writeNotif(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.stdin.Write(data)
	return err
}

// ListTools returns the remote tool catalogue.
func (c *StdioMCPClient) ListTools(ctx context.Context) ([]MCPRemoteTool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []MCPRemoteTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: parse tools/list: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes a remote tool.
func (c *StdioMCPClient) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	raw, err := c.request(ctx, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("mcp: parse tools/call: %w", err)
	}
	var sb strings.Builder
	for _, blk := range out.Content {
		if blk.Type == "text" || blk.Type == "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(blk.Text)
		}
	}
	return sb.String(), !out.IsError, nil
}

// Close terminates the child process and pipes.
func (c *StdioMCPClient) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	return nil
}

// ----------------------------------------------------------------------------
// HTTP transport
// ----------------------------------------------------------------------------

// HTTPMCPClient speaks MCP by POSTing JSON-RPC envelopes to a URL.
type HTTPMCPClient struct {
	url    string
	header map[string]string
	nextID int64
}

// NewHTTPMCPClient connects to an MCP server reachable at the given URL.
// Optional headers (e.g. Authorization) are sent on every request.
func NewHTTPMCPClient(url string, headers map[string]string) (*HTTPMCPClient, error) {
	if url == "" {
		return nil, fmt.Errorf("mcp: url is required for http transport")
	}
	return &HTTPMCPClient{url: url, header: headers}, nil
}

func (c *HTTPMCPClient) doRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.header {
		httpReq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	// Some MCP HTTP servers stream responses as SSE (event: message / data: ...).
	// Extract the last JSON object from any SSE framing.
	body = extractSSEPayload(body)
	var rpc jsonRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("mcp: parse http response: %w (body: %s)", err, strutil.Truncate(string(body), 200))
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("mcp: %s: [%d] %s", method, rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

// extractSSEPayload pulls the JSON payload out of an SSE "data:" line if the
// server framed the response that way; otherwise returns the body unchanged.
func extractSSEPayload(body []byte) []byte {
	s := string(body)
	if !strings.Contains(s, "data:") {
		return body
	}
	var last []byte
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			last = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if last != nil {
		return last
	}
	return body
}

// Initialize performs the MCP handshake over HTTP.
func (c *HTTPMCPClient) Initialize(ctx context.Context) (MCPServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.doRequest(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "darkcode", "version": "1.0.0"},
	})
	if err != nil {
		return MCPServerInfo{}, err
	}
	var info struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		ServerInfo      MCPServerInfo          `json:"serverInfo"`
		Capabilities    map[string]interface{} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return MCPServerInfo{}, fmt.Errorf("mcp: parse initialize: %w", err)
	}
	info.ServerInfo.ProtocolVersion = info.ProtocolVersion
	info.ServerInfo.Capabilities = info.Capabilities
	return info.ServerInfo, nil
}

// ListTools returns the remote tool catalogue over HTTP.
func (c *HTTPMCPClient) ListTools(ctx context.Context) ([]MCPRemoteTool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.doRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []MCPRemoteTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: parse tools/list: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes a remote tool over HTTP.
func (c *HTTPMCPClient) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	raw, err := c.doRequest(ctx, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("mcp: parse tools/call: %w", err)
	}
	var sb strings.Builder
	for _, blk := range out.Content {
		if blk.Type == "text" || blk.Type == "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(blk.Text)
		}
	}
	return sb.String(), !out.IsError, nil
}

// Close is a no-op for the HTTP transport (no persistent connection).
func (c *HTTPMCPClient) Close() error { return nil }

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------
