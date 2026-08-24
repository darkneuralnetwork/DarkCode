package tools

// extension.go — turning a loaded plugin into capability the agent can reach.
//
// # THE DEFECT THIS FIXES
//
// The plugin host has always spawned the binary, completed the manifest and
// init handshake, and stored the process. Nothing ever read the manifest back.
// A plugin declaring three tools loaded successfully, appeared in `/plugins`,
// and was inert: `Host.Execute` had no caller outside its own tests, so no
// declared tool was ever registered and the agent could not call one.
//
// That is the whole extension story up to now — a loading mechanism with no
// second half. This file is the second half.
//
// # WHY THE REGISTRATION LIVES HERE
//
// The same reason MCP's does, a few files over: the tool registry owns what a
// tool is, and a foreign tool has to arrive through the same door as a built-in
// one or it misses the permission gate, the circuit breaker, the spill store
// and the lifecycle hooks. sources.go registers MCP tools exactly this way and
// this mirrors it deliberately.
//
// Commands and hooks are returned rather than registered, because neither is
// the registry's to own — the console owns commands and the hook manager owns
// hooks. Reading them out of the manifest is still this file's job, so a
// caller wires three things from one call instead of parsing manifests itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkcode/tools/plugin"
)

// ExtensionCommand is a slash command a bundle offers.
type ExtensionCommand struct {
	// Name has no leading slash; the console adds it.
	Name        string
	Description string
	// Plugin and Tool say what to run when it is invoked.
	Plugin string
	Tool   string
}

// ExtensionHook is a lifecycle hook a bundle brings with it. The fields mirror
// hooks.Hook exactly; they are carried as plain strings so this package does
// not have to import the hook manager to read a manifest.
type ExtensionHook struct {
	Point   string
	Match   string
	Run     string
	Timeout string
}

// Extensions is everything a set of loaded bundles contributes.
type Extensions struct {
	Tools    []string
	Commands []ExtensionCommand
	Hooks    []ExtensionHook
	// Rejected explains every registration that was declined, so a bundle that
	// half-loads says which half. Silence here used to be indistinguishable
	// from a bundle that declared nothing.
	Rejected []string
}

// pluginHost is the part of *plugin.Host this needs. Narrow so the tests do not
// have to spawn real processes to exercise the mapping.
type pluginHost interface {
	Manifests() []plugin.Manifest
	Execute(pluginPath, tool string, args map[string]interface{}) (string, error)
}

// RegisterExtensions registers every tool the loaded bundles declare and
// returns their commands and hooks for the caller to wire.
//
// A malformed registration costs itself and is reported in Rejected. One bad
// line in a manifest should not cost the other twelve — the same rule the skill
// importer and the plugin loader already apply.
func RegisterExtensions(reg *Registry, host pluginHost) Extensions {
	var out Extensions
	if reg == nil || host == nil {
		return out
	}
	for _, m := range host.Manifests() {
		src := m.Name
		if src == "" {
			src = "extension"
		}
		for _, r := range m.Registers {
			switch r.Type {
			case plugin.ToolType:
				name, err := registerExtensionTool(reg, host, m, r, src)
				if err != nil {
					out.Rejected = append(out.Rejected, fmt.Sprintf("%s: tool %q: %v", src, r.ID, err))
					continue
				}
				out.Tools = append(out.Tools, name)

			case plugin.CommandType:
				name := strings.TrimPrefix(strings.TrimSpace(r.ID), "/")
				if name == "" {
					out.Rejected = append(out.Rejected, src+": a command has no name")
					continue
				}
				out.Commands = append(out.Commands, ExtensionCommand{
					Name: name, Description: r.Description,
					Plugin: m.Path, Tool: commandTool(r),
				})

			case plugin.HookType:
				if strings.TrimSpace(r.Run) == "" {
					out.Rejected = append(out.Rejected, fmt.Sprintf("%s: hook %q has no command to run", src, r.ID))
					continue
				}
				out.Hooks = append(out.Hooks, ExtensionHook{
					Point: r.ID, Match: r.Match, Run: r.Run, Timeout: r.Timeout,
				})

			case plugin.ProviderType:
				// Providers are the model layer's business, not the registry's.
				continue

			default:
				out.Rejected = append(out.Rejected, fmt.Sprintf("%s: unknown registration type %q", src, r.Type))
			}
		}
	}
	return out
}

// commandTool is the tool a command invokes: an explicit one when the manifest
// gave it, else a tool of the same name, which is the shape a bundle almost
// always has.
func commandTool(r plugin.Registration) string {
	if t := strings.TrimSpace(r.Parameters); t != "" && !strings.HasPrefix(t, "{") {
		return t
	}
	return strings.TrimPrefix(strings.TrimSpace(r.ID), "/")
}

func registerExtensionTool(reg *Registry, host pluginHost, m plugin.Manifest, r plugin.Registration, src string) (string, error) {
	name := strings.TrimSpace(r.ID)
	if name == "" {
		return "", fmt.Errorf("no name")
	}
	params, err := extensionSchema(r.Parameters)
	if err != nil {
		return "", err
	}
	// Namespace collisions the same way MCP does, so two bundles exporting
	// "search" both stay callable rather than one silently winning.
	if existing, ok := reg.Get(name); ok && existing.Source != src {
		name = src + "__" + name
	}
	desc := r.Description
	if desc == "" {
		desc = fmt.Sprintf("%s (provided by the %s extension)", r.ID, src)
	}
	reg.Register(&ToolEntry{
		Name:        name,
		Description: desc,
		Parameters:  params,
		Handler:     makeExtensionHandler(host, m.Path, r.ID, name),
		Category:    "extension",
		Source:      src,
	})
	return name, nil
}

// emptySchema is what a tool that declared no parameters gets: an object with
// no properties, which is what every other zero-argument tool in the registry
// carries.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// extensionSchema validates a manifest's declared parameters.
//
// A tool with an unparseable schema is REFUSED rather than registered with an
// empty one. Registering it would tell the model the tool takes no arguments,
// so it would be called wrong every single time and fail in a way that looks
// like the tool is broken rather than the manifest — worse than the tool simply
// being absent.
func extensionSchema(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return emptySchema, nil
	}
	if !strings.HasPrefix(raw, "{") {
		// Not a schema at all — a bundle using Parameters to name its backing
		// tool for a command. Treated as "no parameters" rather than an error.
		return emptySchema, nil
	}
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, fmt.Errorf("parameters are not valid JSON schema: %w", err)
	}
	return json.RawMessage(raw), nil
}

// makeExtensionHandler proxies a call to the plugin process. remoteName is what
// the manifest declared; localName is what was registered, which differs when
// the name collided.
func makeExtensionHandler(host pluginHost, pluginPath, remoteName, localName string) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) *ToolResult {
		// The host's RPC is synchronous and has no context, so cancellation is
		// honoured on the way in rather than mid-call. A hung plugin is bounded
		// by the registry's own per-tool timeout.
		if err := ctx.Err(); err != nil {
			return &ToolResult{Name: localName, Success: false, Error: err.Error()}
		}
		out, err := host.Execute(pluginPath, remoteName, args)
		if err != nil {
			return &ToolResult{Name: localName, Success: false, Error: err.Error()}
		}
		return &ToolResult{Name: localName, Success: true, Output: unwrapJSONString(out)}
	}
}

// unwrapJSONString unquotes a result that is a JSON string.
//
// The host returns the RPC result verbatim, so a plugin answering with a plain
// string hands back `"5 words"` — quotes and escapes included. That text goes
// straight into the model's context, where the quoting is noise at best and a
// mangled path at worst. Anything that is not a JSON string (an object, an
// array, a number) is passed through untouched, because there the structure is
// the answer.
func unwrapJSONString(out string) string {
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, `"`) {
		return out
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
		return out
	}
	return s
}
