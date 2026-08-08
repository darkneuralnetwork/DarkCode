package plugin

import (
	"encoding/json"
)

// RegistrationType defines what a plugin provides.
type RegistrationType string

const (
	ProviderType RegistrationType = "provider"
	ToolType     RegistrationType = "tool"
	// CommandType is a slash command the console offers. A bundle that ships a
	// workflow usually wants one, and without it the workflow is reachable only
	// by asking the agent in prose to call a tool.
	CommandType RegistrationType = "command"
	// HookType is a lifecycle hook the bundle brings with it, so installing an
	// extension can arrange its own capture and gating rather than asking the
	// user to hand-edit config after the fact.
	HookType RegistrationType = "hook"
)

// Manifest describes the plugin's metadata.
type Manifest struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Registers []Registration `json:"registers"`
	// Path is where the bundle was loaded from. The host fills it in after the
	// handshake — it is the host's knowledge, not the bundle's, and a manifest
	// that named its own path could lie about it. Callers need it to route an
	// execute back to the right process.
	Path string `json:"-"`
}

// Registration describes a single exported capability.
//
// One shape covers all four kinds rather than four types with one field each:
// a manifest is read by hand as often as by code, and a reader should not have
// to know which struct a line belongs to.
type Registration struct {
	Type RegistrationType `json:"type"`
	// ID is the tool name, the command name (with or without a leading slash),
	// or the hook point.
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Parameters  string `json:"parameters,omitempty"` // JSON schema string, tools only

	// Run is the shell command for a hook. Hooks deliberately reuse the shape
	// package hooks already validates rather than gaining a second execution
	// backend that calls back into the plugin process: a hook is a one-liner by
	// design, and a bundle shipping one is shipping configuration, not code.
	Run string `json:"run,omitempty"`
	// Match filters a hook by tool name, and Timeout overrides its default.
	Match   string `json:"match,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Plugin defines the interface for external plugins.
type Plugin interface {
	Manifest() (Manifest, error)
	Init() error
	Execute(tool string, args map[string]interface{}) (string, error)
	Shutdown() error
}

// RPCRequest is a JSON-RPC 2.0 request.
type RPCRequest struct {
	ID     int                    `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response.
type RPCResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// RPC methods.
const (
	MethodManifest = "manifest"
	MethodInit     = "init"
	MethodExecute  = "execute"
	MethodShutdown = "shutdown"
)
