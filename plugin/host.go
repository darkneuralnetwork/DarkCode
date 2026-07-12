package plugin

// host.go — the plugin host manages external plugin processes and
// communicates with them via JSON-RPC over stdin/stdout.
//
// Previously Host.Load spawned a binary and immediately discarded it (the
// process was orphaned, no communication happened, and grpcPluginStub
// returned empty manifests). It now:
//   - Spawns the plugin binary with stdin/stdout pipes
//   - Performs a handshake (manifest → init)
//   - Proxies Execute calls via JSON-RPC
//   - Shuts down all plugins cleanly on Host.Shutdown()

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Host represents the main application loading plugins.
type Host struct {
	mu      sync.Mutex
	plugins map[string]*managedPlugin
	nextID  int64
}

// managedPlugin wraps an external process and its stdin/stdout pipes.
type managedPlugin struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	manifest Manifest
}

func NewHost() *Host {
	return &Host{
		plugins: make(map[string]*managedPlugin),
	}
}

// Load spawns a plugin binary and performs the manifest/init handshake.
func (h *Host) Load(binaryPath string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.plugins[binaryPath]; ok {
		return nil // already loaded
	}

	cmd := exec.Command(binaryPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe for %s: %w", binaryPath, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe for %s: %w", binaryPath, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start plugin %s: %w", binaryPath, err)
	}

	mp := &managedPlugin{
		name:   binaryPath,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}

	// Handshake: manifest.
	manifest, err := mp.call(h.nextReqID(), MethodManifest, nil)
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("plugin %s manifest handshake failed: %w", binaryPath, err)
	}
	if err := json.Unmarshal(manifest, &mp.manifest); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("plugin %s manifest parse failed: %w", binaryPath, err)
	}

	// Handshake: init.
	if _, err := mp.call(h.nextReqID(), MethodInit, nil); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("plugin %s init failed: %w", binaryPath, err)
	}

	h.plugins[binaryPath] = mp
	return nil
}

// Execute calls a tool on the named plugin.
func (h *Host) Execute(pluginPath, tool string, args map[string]interface{}) (string, error) {
	h.mu.Lock()
	mp, ok := h.plugins[pluginPath]
	h.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("plugin not loaded: %s", pluginPath)
	}
	params := map[string]interface{}{"tool": tool, "args": args}
	result, err := mp.call(h.nextReqID(), MethodExecute, params)
	if err != nil {
		return "", err
	}
	return string(result), nil
}

// Manifests returns the manifests of all loaded plugins.
func (h *Host) Manifests() []Manifest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Manifest, 0, len(h.plugins))
	for _, mp := range h.plugins {
		out = append(out, mp.manifest)
	}
	return out
}

// Shutdown gracefully shuts down all loaded plugins.
func (h *Host) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for path, mp := range h.plugins {
		_, _ = mp.call(h.nextReqID(), MethodShutdown, nil)
		_ = mp.stdin.Close()
		_ = mp.cmd.Process.Kill()
		_ = mp.cmd.Wait()
		delete(h.plugins, path)
	}
}

// nextReqID returns a unique request ID.
func (h *Host) nextReqID() int {
	return int(atomic.AddInt64(&h.nextID, 1))
}

// call sends a JSON-RPC request and reads the response.
func (mp *managedPlugin) call(id int, method string, params map[string]interface{}) (json.RawMessage, error) {
	req := RPCRequest{ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := mp.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write to plugin: %w", err)
	}

	line, err := mp.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read from plugin: %w", err)
	}

	var resp RPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse plugin response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("plugin error: %s", resp.Error)
	}
	return resp.Result, nil
}
