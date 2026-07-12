package tools

// ============================================================================
// TOOL SOURCE MANAGER
//
// Manages external/in-house tool sources that can be connected and
// disconnected from the running agent at runtime, in both the CLI and the GUI.
//
// A "tool source" is anything that contributes tools to the registry:
//
//   • mcp_stdio — a local MCP server spawned as a child process
//   • mcp_http  — a remote MCP server reached over HTTP
//   • internal  — one or more in-house tools defined in the Internal Tool
//                 Format (see itf.go + format.txt)
//
// When a source is connected, its tools are registered in the shared
// Registry (with the source id stamped on each ToolEntry.Source) so the agent
// can discover and call them. When disconnected, those tools are removed and
// the underlying connection/process is closed. Sources are identified by a
// stable string id (auto-generated if not provided) and their definitions
// can be persisted to .config so they auto-reconnect on the next launch.
// ============================================================================

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SourceType identifies how a source's tools are obtained.
type SourceType string

const (
	SourceMCPStdio SourceType = "mcp_stdio"
	SourceMCPHTTP  SourceType = "mcp_http"
	SourceInternal SourceType = "internal"
	SourceHTP      SourceType = "htp" // remote DarkCode Tool Protocol server
)

// SourceStatus is the current lifecycle state of a source.
type SourceStatus string

const (
	StatusDisconnected SourceStatus = "disconnected"
	StatusConnecting   SourceStatus = "connecting"
	StatusConnected    SourceStatus = "connected"
	StatusError        SourceStatus = "error"
)

// SourceConfig is the persistable definition of a tool source. It mirrors
// config.ToolSourceConfig so the two can be converted trivially.
type SourceConfig struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        SourceType        `json:"type"`
	Command     string            `json:"command,omitempty"` // mcp_stdio
	Args        []string          `json:"args,omitempty"`    // mcp_stdio
	Env         map[string]string `json:"env,omitempty"`     // mcp_stdio
	URL         string            `json:"url,omitempty"`     // mcp_http
	Headers     map[string]string `json:"headers,omitempty"` // mcp_http
	Path        string            `json:"path,omitempty"`    // internal (file or dir)
	AutoConnect bool              `json:"auto_connect,omitempty"`
}

// Source is a runtime view of a tool source: its config plus live state.
type Source struct {
	Config     SourceConfig `json:"config"`
	Status     SourceStatus `json:"status"`
	Tools      []string     `json:"tools"`                 // names of tools currently registered
	ServerInfo string       `json:"server_info,omitempty"` // MCP server name@version
	Error      string       `json:"error,omitempty"`
	UpdatedAt  time.Time    `json:"updated_at"`

	// runtime-only fields (not serialized)
	client MCPClient `json:"-"`
}

// SourceManager owns the set of tool sources and mediates their interaction
// with the shared registry. It is safe for concurrent use.
type SourceManager struct {
	mu       sync.RWMutex
	registry *Registry
	sources  map[string]*Source
}

// NewSourceManager creates a manager bound to the given registry. The
// registry is where connected tools are published.
func NewSourceManager(reg *Registry) *SourceManager {
	return &SourceManager{
		registry: reg,
		sources:  make(map[string]*Source),
	}
}

// Registry returns the registry this manager publishes tools into.
func (m *SourceManager) Registry() *Registry { return m.registry }

// Add registers a source definition WITHOUT connecting it. If the config has
// no id, one is generated. Returns the (possibly new) id.
func (m *SourceManager) Add(cfg SourceConfig) (string, error) {
	if err := validateSourceConfig(cfg); err != nil {
		return "", err
	}
	if cfg.ID == "" {
		cfg.ID = generateSourceID(cfg)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sources[cfg.ID]; exists {
		return "", fmt.Errorf("source %q already exists", cfg.ID)
	}
	m.sources[cfg.ID] = &Source{
		Config:    cfg,
		Status:    StatusDisconnected,
		UpdatedAt: time.Now(),
	}
	return cfg.ID, nil
}

// validateSourceConfig checks the fields required for each source type.
func validateSourceConfig(cfg SourceConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("source name is required")
	}
	switch cfg.Type {
	case SourceMCPStdio:
		if cfg.Command == "" {
			return fmt.Errorf("command is required for mcp_stdio sources")
		}
	case SourceMCPHTTP:
		if cfg.URL == "" {
			return fmt.Errorf("url is required for mcp_http sources")
		}
	case SourceInternal:
		if cfg.Path == "" {
			return fmt.Errorf("path is required for internal sources")
		}
	case SourceHTP:
		if cfg.URL == "" {
			return fmt.Errorf("url is required for htp sources (remote HTP server base URL)")
		}
	default:
		return fmt.Errorf("unknown source type %q (use mcp_stdio, mcp_http, internal, or htp)", cfg.Type)
	}
	return nil
}

// generateSourceID builds a stable-ish id from the source name + type.
func generateSourceID(cfg SourceConfig) string {
	base := slug(cfg.Name)
	if base == "" {
		base = string(cfg.Type)
	}
	return base + "-" + randSuffix(6)
}

// Connect brings a source online: it opens the connection (MCP) or loads the
// definitions (internal), discovers tools, and registers them. Tools are
// stamped with the source id so they can be removed on disconnect.
func (m *SourceManager) Connect(ctx context.Context, id string) error {
	m.mu.Lock()
	src, ok := m.sources[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such source: %s", id)
	}

	m.setSourceStatus(id, StatusConnecting, "")

	var (
		tools   []string
		srvInfo string
		client  MCPClient
		err     error
	)
	switch src.Config.Type {
	case SourceMCPStdio:
		tools, srvInfo, client, err = m.connectMCPStdio(ctx, src)
	case SourceMCPHTTP:
		tools, srvInfo, client, err = m.connectMCPHTTP(ctx, src)
	case SourceInternal:
		tools, err = m.connectInternal(src)
		client = nil
	case SourceHTP:
		tools, srvInfo, err = m.connectHTP(ctx, src)
		client = nil
	}
	if err != nil {
		m.setSourceStatus(id, StatusError, err.Error())
		return err
	}

	m.mu.Lock()
	src.Tools = tools
	src.ServerInfo = srvInfo
	src.client = client
	src.Status = StatusConnected
	src.Error = ""
	src.UpdatedAt = time.Now()
	m.mu.Unlock()
	return nil
}

// connectMCPStdio spawns the MCP server, handshakes, and registers tools.
func (m *SourceManager) connectMCPStdio(ctx context.Context, src *Source) ([]string, string, MCPClient, error) {
	client, err := NewStdioMCPClient(src.Config.Command, src.Config.Args, src.Config.Env)
	if err != nil {
		return nil, "", nil, err
	}
	info, err := client.Initialize(ctx)
	if err != nil {
		client.Close()
		return nil, "", nil, fmt.Errorf("initialize: %w", err)
	}
	remote, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return nil, "", nil, fmt.Errorf("tools/list: %w", err)
	}
	tools := m.registerMCPTools(src.Config.ID, remote, client)
	return tools, info.Name + "@" + info.Version, client, nil
}

// connectMCPHTTP dials the MCP HTTP endpoint and registers its tools.
func (m *SourceManager) connectMCPHTTP(ctx context.Context, src *Source) ([]string, string, MCPClient, error) {
	client, err := NewHTTPMCPClient(src.Config.URL, src.Config.Headers)
	if err != nil {
		return nil, "", nil, err
	}
	info, err := client.Initialize(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("initialize: %w", err)
	}
	remote, err := client.ListTools(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("tools/list: %w", err)
	}
	tools := m.registerMCPTools(src.Config.ID, remote, client)
	return tools, info.Name + "@" + info.Version, client, nil
}

// registerMCPTools turns each remote MCP tool into a local ToolEntry whose
// handler proxies CallTool to the server. Returns the registered names.
func (m *SourceManager) registerMCPTools(sourceID string, remote []MCPRemoteTool, client MCPClient) []string {
	var names []string
	for _, rt := range remote {
		name := rt.Name
		// Namespace collisions: if a tool with this name already exists from
		// another source, prefix with the source id to keep both callable.
		if existing, ok := m.registry.Get(name); ok && existing.Source != sourceID {
			name = sourceID + "__" + rt.Name
		}
		params := mcpSchemaToParameters(rt.InputSchema)
		handler := makeMCPHandler(name, rt.Name, client)
		m.registry.Register(&ToolEntry{
			Name:        name,
			Description: rt.Description,
			Parameters:  params,
			Handler:     handler,
			Category:    "mcp",
			Source:      sourceID,
		})
		names = append(names, name)
	}
	return names
}

// makeMCPHandler builds a ToolHandler that proxies to an MCP server's
// tools/call. The remoteName is the original tool name on the server; the
// localName is what we registered (may be namespaced).
func makeMCPHandler(localName, remoteName string, client MCPClient) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) *ToolResult {
		out, ok, err := client.CallTool(ctx, remoteName, args)
		if err != nil {
			return &ToolResult{Name: localName, Success: false, Error: err.Error()}
		}
		res := &ToolResult{Name: localName, Output: out, Success: ok}
		if !ok {
			res.Error = "MCP tool reported an error"
		}
		return res
	}
}

// mcpSchemaToParameters converts an MCP inputSchema (map) into the
// json.RawMessage the registry expects. Falls back to an empty object.
func mcpSchemaToParameters(schema map[string]interface{}) json.RawMessage {
	if len(schema) == 0 {
		return MustParseSchema(`{"type":"object","properties":{}}`)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return MustParseSchema(`{"type":"object","properties":{}}`)
	}
	return json.RawMessage(b)
}

// connectInternal loads ITF tool definitions from a file or directory and
// registers them.
func (m *SourceManager) connectInternal(src *Source) ([]string, error) {
	tools, err := loadITFFromPath(src.Config.Path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range tools {
		name := t.Name
		if existing, ok := m.registry.Get(name); ok && existing.Source != src.Config.ID {
			name = src.Config.ID + "__" + t.Name
		}
		entry := ITFToolEntry(t)
		entry.Name = name
		entry.Source = src.Config.ID
		m.registry.Register(entry)
		names = append(names, name)
	}
	return names, nil
}

// connectHTP discovers tools from a remote DarkCode Tool Protocol server and
// registers each as a local ITF-"htp" tool that calls back into the server.
// This is the in-house-format path for connecting OUTER or REMOTE devices:
// the device runs a tiny HTP server (server/htp.go) exposing its tools; the
// agent discovers and calls them like any builtin. On disconnect the tools
// are removed (the shared Disconnect path handles that via Source stamping).
func (m *SourceManager) connectHTP(ctx context.Context, src *Source) ([]string, string, error) {
	baseURL := src.Config.URL
	if baseURL == "" {
		return nil, "", fmt.Errorf("htp source requires a url")
	}
	remoteTools, err := HTPDiscover(ctx, baseURL, src.Config.Headers)
	if err != nil {
		return nil, "", fmt.Errorf("discover %s: %w", baseURL, err)
	}
	entries := HTPRemoteToolEntries(baseURL, src.Config.Headers, remoteTools, "")
	var names []string
	for _, entry := range entries {
		name := entry.Name
		if existing, ok := m.registry.Get(name); ok && existing.Source != src.Config.ID {
			name = src.Config.ID + "__" + entry.Name
			entry.Name = name
		}
		entry.Source = src.Config.ID
		m.registry.Register(entry)
		names = append(names, name)
	}
	srvInfo := fmt.Sprintf("htp server %s: %d tools", baseURL, len(names))
	return names, srvInfo, nil
}

// loadITFFromPath loads ITF definitions from a single file or every *.json
// file in a directory (one level deep).
func loadITFFromPath(path string) ([]ITFTool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("internal source path: %w", err)
	}
	if !info.IsDir() {
		return loadITFFile(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	var all []ITFTool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".json" && filepath.Ext(name) != ".itf" {
			continue
		}
		tools, err := loadITFFile(filepath.Join(path, name))
		if err != nil {
			// Skip malformed files but keep going; surface a partial result.
			continue
		}
		all = append(all, tools...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no ITF tools found in %s", path)
	}
	return all, nil
}

// loadITFFile parses a single ITF document.
func loadITFFile(path string) ([]ITFTool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_, tools, err := ParseITF(data)
	return tools, err
}

// Disconnect takes a source offline: its tools are removed from the registry
// and the underlying connection/process is closed. The source definition is
// retained so it can be reconnected later.
func (m *SourceManager) Disconnect(id string) error {
	m.mu.Lock()
	src, ok := m.sources[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no such source: %s", id)
	}
	client := src.client
	m.mu.Unlock()

	removed := m.registry.UnregisterBySource(id)
	if client != nil {
		_ = client.Close()
	}

	m.mu.Lock()
	src.client = nil
	src.Tools = nil
	src.Status = StatusDisconnected
	src.Error = ""
	src.UpdatedAt = time.Now()
	m.mu.Unlock()
	_ = removed
	return nil
}

// Remove disconnects (if needed) and deletes the source definition entirely.
func (m *SourceManager) Remove(id string) error {
	m.mu.Lock()
	_, ok := m.sources[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such source: %s", id)
	}
	// Best-effort disconnect first.
	_ = m.Disconnect(id)
	m.mu.Lock()
	delete(m.sources, id)
	m.mu.Unlock()
	return nil
}

// Get returns a snapshot of a single source.
func (m *SourceManager) Get(id string) (Source, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src, ok := m.sources[id]
	if !ok {
		return Source{}, false
	}
	return *src, true
}

// List returns snapshots of all known sources.
func (m *SourceManager) List() []Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Source, 0, len(m.sources))
	for _, src := range m.sources {
		out = append(out, *src)
	}
	return out
}

// Configs returns the persistable configs for all sources (for .config).
func (m *SourceManager) Configs() []SourceConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SourceConfig, 0, len(m.sources))
	for _, src := range m.sources {
		out = append(out, src.Config)
	}
	return out
}

// setSourceStatus updates a source's status/error and timestamp.
func (m *SourceManager) setSourceStatus(id string, status SourceStatus, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if src, ok := m.sources[id]; ok {
		src.Status = status
		src.Error = errMsg
		src.UpdatedAt = time.Now()
	}
}

// ConnectAll connects every source whose AutoConnect flag is set. Used at
// startup to restore previously-registered sources from .config. Errors are
// collected per-source and returned as a single error (nil if all succeeded).
func (m *SourceManager) ConnectAll(ctx context.Context) error {
	m.mu.RLock()
	ids := make([]string, 0)
	for id, src := range m.sources {
		if src.Config.AutoConnect {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	var errs []string
	for _, id := range ids {
		if err := m.Connect(ctx, id); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("some sources failed to connect: %s", joinStrings(errs, "; "))
	}
	return nil
}

// Helper: minimal slug generator for source ids.
func slug(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	// Collapse runs of dashes and trim edges.
	res := make([]byte, 0, len(out))
	prevDash := false
	for _, b := range out {
		if b == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		res = append(res, b)
	}
	for len(res) > 0 && res[0] == '-' {
		res = res[1:]
	}
	for len(res) > 0 && res[len(res)-1] == '-' {
		res = res[:len(res)-1]
	}
	if len(res) > 40 {
		res = res[:40]
	}
	return string(res)
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// sourceIDCounter ensures generated source ids are unique within a process.
var sourceIDCounter int64

// randSuffix returns a short, process-unique alphanumeric suffix used when a
// source has no explicit id. It combines a monotonic counter with a time-based
// component so ids are effectively unique without pulling in crypto/rand.
func randSuffix(n int) string {
	c := atomic.AddInt64(&sourceIDCounter, 1)
	base := strconv.FormatInt(time.Now().UnixNano()%100000+c, 36)
	if len(base) > n {
		base = base[len(base)-n:]
	}
	for len(base) < n {
		base = "0" + base
	}
	return base
}
