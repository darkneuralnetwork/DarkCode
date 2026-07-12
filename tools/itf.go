package tools

// ============================================================================
// INTERNAL TOOL FORMAT (ITF) — v1.0
//
// A JSON-based format for defining in-house tools that are loaded into the
// agent's tool registry at runtime. Each ITF document describes one or more
// tools: their JSON-Schema parameters and an "execution" block that tells the
// runtime how to actually run them.
//
// Three execution kinds are supported:
//
//   1. "command" — render a shell command template with {{arg}} placeholders
//      and execute it (like the built-in terminal tool, but scoped to a
//      single, declared command).
//   2. "http"     — render method/url/headers/body templates and perform an
//      HTTP request, returning the response body.
//   3. "static"   — return a fixed string. Useful for stubs, fixtures, and
//      documenting "no-op" tools.
//
// Placeholders: any {{name}} token inside a command/url/body string or a
// header value is replaced with the string form of the matching argument.
// Unknown placeholders are replaced with the empty string. Values are shell/
// JSON-safe escaped depending on context (command → shell-quoted, http body →
// left as-is since it is JSON-encoded into the request).
//
// See format.txt at the project root for the full specification and examples.
// ============================================================================

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ITFDocument is the top-level structure of an Internal Tool Format file.
// It may carry a single tool (via "tool") or a set of tools (via "tools").
type ITFDocument struct {
	Format  string    `json:"format"`  // must be "darkcode-itf"
	Version string    `json:"version"` // "1.0"
	Name    string    `json:"name"`    // toolset name (informational)
	Tool    *ITFTool  `json:"tool,omitempty"`
	Tools   []ITFTool `json:"tools,omitempty"`
}

// ITFTool describes a single in-house tool.
type ITFTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category,omitempty"`
	Destructive bool            `json:"destructive,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Execution   ITFExecution    `json:"execution"`
}

// ITFExecution is the execution block. Exactly one of the kind-specific
// fields is meaningful, selected by Type.
type ITFExecution struct {
	Type    string `json:"type"` // command | http | static | htp
	Command string `json:"command,omitempty"`
	Shell   string `json:"shell,omitempty"`   // bash (default) | sh
	Workdir string `json:"workdir,omitempty"` // working directory for command
	Timeout int    `json:"timeout,omitempty"` // seconds (command/http/htp)

	Method  string            `json:"method,omitempty"`  // http: GET/POST/...
	URL     string            `json:"url,omitempty"`     // http: url template | htp: base URL of the remote HTP server
	Headers map[string]string `json:"headers,omitempty"` // http/htp: header templates (e.g. auth)
	Body    string            `json:"body,omitempty"`    // http: body template

	// htp: the remote tool name to invoke. If empty, the ITF tool's own Name
	// is used. This lets an ITF document remap a remote tool under a local name.
	RemoteTool string `json:"remote_tool,omitempty"`

	Output string `json:"output,omitempty"` // static: fixed output
}

// ParseITF parses an ITF document from raw JSON. It validates the format
// marker and returns the flat list of tool definitions (whether the document
// used the single "tool" field or the "tools" array).
func ParseITF(data []byte) (*ITFDocument, []ITFTool, error) {
	var doc ITFDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("itf: invalid JSON: %w", err)
	}
	if doc.Format != "" && doc.Format != "darkcode-itf" {
		return nil, nil, fmt.Errorf("itf: unknown format %q (expected \"darkcode-itf\")", doc.Format)
	}
	tools := doc.Tools
	if doc.Tool != nil {
		tools = append([]ITFTool{*doc.Tool}, tools...)
	}
	if len(tools) == 0 {
		return nil, nil, fmt.Errorf("itf: no tools defined (use \"tool\" or \"tools\")")
	}
	for i := range tools {
		if err := validateITFTool(&tools[i]); err != nil {
			return nil, nil, fmt.Errorf("itf: tool %q: %w", tools[i].Name, err)
		}
	}
	return &doc, tools, nil
}

// validateITFTool checks that a tool definition is well-formed.
func validateITFTool(t *ITFTool) error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(t.Parameters) == 0 {
		// Default to an empty object schema so the tool takes no args.
		t.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	} else {
		// Validate it parses as JSON.
		var v interface{}
		if err := json.Unmarshal(t.Parameters, &v); err != nil {
			return fmt.Errorf("parameters is not valid JSON: %w", err)
		}
	}
	switch t.Execution.Type {
	case "command":
		if t.Execution.Command == "" {
			return fmt.Errorf("execution.command is required for type \"command\"")
		}
	case "http":
		if t.Execution.URL == "" {
			return fmt.Errorf("execution.url is required for type \"http\"")
		}
		if t.Execution.Method == "" {
			t.Execution.Method = "GET"
		}
	case "static":
		// Output may be empty (returns empty string). Nothing to validate.
	case "htp":
		// Remote HTP tool: calls a remote DarkCode Tool Protocol server's
		// "execute" action. Requires the server base URL; the remote tool name
		// defaults to this tool's Name if remote_tool is empty.
		if t.Execution.URL == "" {
			return fmt.Errorf("execution.url is required for type \"htp\" (the remote HTP server base URL)")
		}
	default:
		return fmt.Errorf("execution.type must be command, http, or static (got %q)", t.Execution.Type)
	}
	if t.Category == "" {
		t.Category = "custom"
	}
	return nil
}

// ITFToolEntry wraps a parsed ITF tool into a registry ToolEntry, wiring the
// handler to the declared execution kind.
func ITFToolEntry(t ITFTool) *ToolEntry {
	return &ToolEntry{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  t.Parameters,
		Category:    t.Category,
		Handler:     newITFHandler(t),
	}
}

// newITFHandler builds a ToolHandler for the given ITF tool definition.
func newITFHandler(t ITFTool) ToolHandler {
	switch t.Execution.Type {
	case "command":
		return itfCommandHandler(t)
	case "http":
		return itfHTTPHandler(t)
	case "htp":
		return itfHTPHandler(t)
	case "static":
		return func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{
				Name:    t.Name,
				Success: true,
				Output:  renderTemplate(t.Execution.Output, args),
			}
		}
	default:
		return func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: t.Name, Success: false, Error: "unknown execution type: " + t.Execution.Type}
		}
	}
}

// itfCommandHandler renders the command template and runs it in a shell.
func itfCommandHandler(t ITFTool) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) *ToolResult {
		// Shell-quote each argument value, then render the template. We pass
		// the already-quoted values so the template author writes the literal
		// {{name}} token (no manual quoting needed).
		quoted := make(map[string]interface{}, len(args))
		for k, v := range args {
			quoted[k] = shellQuote(toStr(v))
		}
		command := renderTemplate(t.Execution.Command, quoted)

		timeout := t.Execution.Timeout
		if timeout <= 0 {
			timeout = 120
		}
		toolCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		shell := t.Execution.Shell
		if shell == "" {
			shell = "bash"
		}
		if _, err := exec.LookPath(shell); err != nil {
			shell = "sh"
		}
		cmd := exec.CommandContext(toolCtx, shell, "-c", command)
		if t.Execution.Workdir != "" {
			cmd.Dir = t.Execution.Workdir
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		output := stdout.String()
		if stderr.Len() > 0 {
			output = strings.TrimRight(output, "\n") + "\n" + stderr.String()
		}
		res := &ToolResult{Name: t.Name, Output: strings.TrimSpace(output)}
		if err != nil {
			if toolCtx.Err() == context.DeadlineExceeded {
				res.Success = false
				res.Error = fmt.Sprintf("command timed out after %ds", timeout)
			} else if exitErr, ok := err.(*exec.ExitError); ok {
				res.Success = false
				res.Error = fmt.Sprintf("exit code %d", exitErr.ExitCode())
			} else {
				res.Success = false
				res.Error = err.Error()
			}
			return res
		}
		res.Success = true
		return res
	}
}

// itfHTTPHandler renders the request templates and performs the HTTP call.
func itfHTTPHandler(t ITFTool) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) *ToolResult {
		method := t.Execution.Method
		if method == "" {
			method = "GET"
		}
		url := renderTemplate(t.Execution.URL, args)
		body := renderTemplate(t.Execution.Body, args)

		timeout := t.Execution.Timeout
		if timeout <= 0 {
			timeout = 60
		}
		toolCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(toolCtx, method, url, bodyReader)
		if err != nil {
			return &ToolResult{Name: t.Name, Success: false, Error: "bad request: " + err.Error()}
		}
		for k, v := range t.Execution.Headers {
			req.Header.Set(k, renderTemplate(v, args))
		}
		if body != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return &ToolResult{Name: t.Name, Success: false, Error: err.Error()}
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
		res := &ToolResult{Name: t.Name, Output: string(data)}
		if resp.StatusCode >= 400 {
			res.Success = false
			res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			res.Success = true
		}
		return res
	}
}

// renderTemplate replaces every {{name}} token with the string form of the
// matching value from args. Unknown names become empty strings.
func renderTemplate(tmpl string, args map[string]interface{}) string {
	// Fast path: no placeholders.
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	var sb strings.Builder
	i := 0
	for {
		start := strings.Index(tmpl[i:], "{{")
		if start < 0 {
			sb.WriteString(tmpl[i:])
			break
		}
		sb.WriteString(tmpl[i : i+start])
		j := i + start + 2
		end := strings.Index(tmpl[j:], "}}")
		if end < 0 {
			sb.WriteString(tmpl[i+start:])
			break
		}
		key := strings.TrimSpace(tmpl[j : j+end])
		sb.WriteString(toStr(args[key]))
		i = j + end + 2
	}
	return sb.String()
}

// shellQuote wraps a string in single quotes for safe shell interpolation,
// escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toStr coerces an interface{} (typically from JSON-decoded args) to a string.
func toStr(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case float64:
		// Print integers without a decimal part.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case json.Number:
		return x.String()
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// ── HTP (DarkCode Tool Protocol) execution ─────────────────────────────────
//
// An ITF tool with execution.type="htp" calls a remote HTP server's
// "execute" action. This is the in-house format for connecting OUTER or
// REMOTE devices: a device on the network runs a tiny HTP server (see
// server/htp.go) and exposes its tools; an ITF document (or the source
// manager's auto-discovery) registers those tools locally so the agent can
// call them like any builtin. The args the LLM passes are forwarded verbatim
// to the remote tool.

// itfHTPHandler builds a handler that calls a remote HTP server's execute
// action for the declared remote tool.
func itfHTPHandler(t ITFTool) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) *ToolResult {
		baseURL := t.Execution.URL
		remoteTool := t.Execution.RemoteTool
		if remoteTool == "" {
			remoteTool = t.Name
		}
		timeout := t.Execution.Timeout
		if timeout <= 0 {
			timeout = 60
		}
		out, err := HTPExecute(ctx, baseURL, remoteTool, args, t.Execution.Headers, timeout)
		res := &ToolResult{Name: t.Name, Output: out}
		if err != nil {
			res.Success = false
			res.Error = err.Error()
		} else {
			res.Success = true
		}
		return res
	}
}

// HTPExecute calls a remote HTP server's "execute" action and returns the
// raw result text. Used by itfHTPHandler and the source manager's
// auto-discovery. baseURL is the server root (e.g. http://device:7788).
func HTPExecute(ctx context.Context, baseURL, tool string, args map[string]interface{}, headers map[string]string, timeoutSec int) (string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	payload := map[string]interface{}{
		"htp_version": "1.0",
		"action":      "execute",
		"tool":        tool,
		"args":        args,
	}
	body, _ := json.Marshal(payload)
	// The HTP server serves its endpoint at /api/htp (see server/htp.go wiring).
	url := strings.TrimRight(baseURL, "/") + "/api/htp"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("htp: bad request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("htp: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return string(data), fmt.Errorf("htp: HTTP %d", resp.StatusCode)
	}
	// Parse the HTP response envelope; extract the result.output field.
	var env struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Result struct {
			Name    string `json:"name"`
			Success bool   `json:"success"`
			Output  string `json:"output"`
			Err     string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return string(data), nil // not an envelope; return raw
	}
	if !env.OK {
		return "", fmt.Errorf("htp: remote error: %s", env.Error)
	}
	if !env.Result.Success && env.Result.Err != "" {
		return env.Result.Output, fmt.Errorf("htp: tool error: %s", env.Result.Err)
	}
	if env.Result.Output != "" {
		return env.Result.Output, nil
	}
	return string(env.Result.Output), nil
}

// HTPListTool describes a remote tool discovered from an HTP server's
// "list" action. It mirrors the fields needed to register the tool locally.
type HTPListTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// HTPDiscover queries a remote HTP server's "list" action and returns the
// tools it exposes. The source manager uses this to auto-register every
// remote tool as a local ITF-"htp" tool — the "connect outer/remote devices"
// flow.
func HTPDiscover(ctx context.Context, baseURL string, headers map[string]string) ([]HTPListTool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload := map[string]interface{}{
		"htp_version": "1.0",
		"action":      "list",
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(baseURL, "/") + "/api/htp"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("htp discover: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("htp discover: HTTP %d: %s", resp.StatusCode, string(data))
	}
	var env struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("htp discover: bad envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("htp discover: %s", env.Error)
	}
	// The list action nests tools under result: {tools:[...], count:n}.
	var listResp struct {
		Tools []HTPListTool `json:"tools"`
	}
	if err := json.Unmarshal(env.Result, &listResp); err != nil {
		return nil, fmt.Errorf("htp discover: bad result: %w", err)
	}
	return listResp.Tools, nil
}

// HTPRemoteToolEntries builds registry ToolEntries for every tool a remote
// HTP server exposes, each wired to call back into that server. This is how a
// remote device contributes tools to the local registry through the in-house
// format. A namePrefix avoids collisions with builtin tools.
func HTPRemoteToolEntries(baseURL string, headers map[string]string, tools []HTPListTool, namePrefix string) []*ToolEntry {
	var out []*ToolEntry
	for _, t := range tools {
		name := t.Name
		if namePrefix != "" {
			name = namePrefix + "_" + t.Name
		}
		cat := t.Category
		if cat == "" {
			cat = "remote"
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		itfTool := ITFTool{
			Name:        name,
			Description: t.Description,
			Category:    cat,
			Parameters:  params,
			Execution: ITFExecution{
				Type:       "htp",
				URL:        baseURL,
				Headers:    headers,
				RemoteTool: t.Name, // call the remote tool by its original name
				Timeout:    60,
			},
		}
		out = append(out, ITFToolEntry(itfTool))
	}
	return out
}
