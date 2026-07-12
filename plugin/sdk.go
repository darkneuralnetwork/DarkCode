package plugin

// sdk.go — the plugin SDK: manifest, interface, and protocol types.
//
// Plugins are external binaries that communicate with the host via JSON-RPC
// over stdin/stdout. This is a zero-dependency alternative to gRPC-based
// plugin systems (spec §11). The protocol is:
//
//   Host → Plugin:  {"id":1,"method":"manifest","params":{}}
//   Plugin → Host:  {"id":1,"result":{"name":"...","version":"...","registers":[...]}}
//
//   Host → Plugin:  {"id":2,"method":"init","params":{}}
//   Plugin → Host:  {"id":2,"result":{}}
//
//   Host → Plugin:  {"id":3,"method":"execute","params":{"tool":"my_tool","args":{...}}}
//   Plugin → Host:  {"id":3,"result":{"output":"...","success":true}}
//
//   Host → Plugin:  {"id":4,"method":"shutdown","params":{}}
//   Plugin → Host:  {"id":4,"result":{}}

import (
	"encoding/json"
)

// RegistrationType defines what a plugin provides.
type RegistrationType string

const (
	ProviderType RegistrationType = "provider"
	ToolType     RegistrationType = "tool"
)

// Manifest describes the plugin's metadata.
type Manifest struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Registers []Registration `json:"registers"`
}

// Registration describes a single exported capability.
type Registration struct {
	Type       RegistrationType `json:"type"`
	ID         string           `json:"id"`
	Parameters string           `json:"parameters,omitempty"` // JSON schema string
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
