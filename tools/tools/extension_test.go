package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/darkcode/tools/plugin"
)

type fakeHost struct {
	manifests []plugin.Manifest
	calls     []string
	reply     string
	err       error
}

func (f *fakeHost) Manifests() []plugin.Manifest { return f.manifests }
func (f *fakeHost) Execute(path, tool string, args map[string]interface{}) (string, error) {
	f.calls = append(f.calls, path+"|"+tool+"|"+fmt.Sprint(args["input"]))
	if f.err != nil {
		return "", f.err
	}
	if f.reply == "" {
		return "ran", nil
	}
	return f.reply, nil
}

func bundle(name string, regs ...plugin.Registration) plugin.Manifest {
	return plugin.Manifest{Name: name, Version: "1", Path: "/x/" + name, Registers: regs}
}

// TestADeclaredToolBecomesCallable is the defect this file exists for. The host
// loaded the bundle and stored it; nothing read the manifest back, so a
// declared tool was never registered and the agent could not call it.
func TestADeclaredToolBecomesCallable(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{reply: "from the bundle", manifests: []plugin.Manifest{
		bundle("acme", plugin.Registration{Type: plugin.ToolType, ID: "deploy",
			Description: "deploy the thing",
			Parameters:  `{"type":"object","properties":{"env":{"type":"string"}}}`}),
	}}

	ext := RegisterExtensions(reg, host)
	if len(ext.Tools) != 1 || ext.Tools[0] != "deploy" {
		t.Fatalf("registered %v, want [deploy]", ext.Tools)
	}
	entry, ok := reg.Get("deploy")
	if !ok {
		t.Fatal("the declared tool is not in the registry")
	}
	if entry.Description != "deploy the thing" || entry.Category != "extension" {
		t.Errorf("entry = %+v, want the manifest's description and the extension category", entry)
	}

	res, err := reg.Execute(context.Background(), "deploy", map[string]interface{}{"env": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Output != "from the bundle" {
		t.Errorf("result = %+v, want the plugin's output", res)
	}
	if len(host.calls) != 1 || !strings.HasPrefix(host.calls[0], "/x/acme|deploy|") {
		t.Errorf("the call went to %v, want the right process and tool", host.calls)
	}
}

// TestCommandsAndHooksAreReturnedNotRegistered — neither is the registry's to
// own, and the console and hook manager would otherwise each parse manifests.
func TestCommandsAndHooksAreReturnedNotRegistered(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{manifests: []plugin.Manifest{
		bundle("acme",
			plugin.Registration{Type: plugin.CommandType, ID: "/shipit", Description: "ship"},
			plugin.Registration{Type: plugin.HookType, ID: "post_tool", Run: "echo done", Match: "write_*"},
		),
	}}

	ext := RegisterExtensions(reg, host)
	if len(ext.Commands) != 1 {
		t.Fatalf("got %d commands, want 1", len(ext.Commands))
	}
	c := ext.Commands[0]
	if c.Name != "shipit" {
		t.Errorf("command name = %q, want the leading slash stripped", c.Name)
	}
	if c.Plugin != "/x/acme" || c.Tool != "shipit" {
		t.Errorf("command routes to %s/%s, want /x/acme/shipit", c.Plugin, c.Tool)
	}
	if len(ext.Hooks) != 1 || ext.Hooks[0].Point != "post_tool" || ext.Hooks[0].Match != "write_*" {
		t.Errorf("hooks = %+v", ext.Hooks)
	}
	if len(ext.Tools) != 0 {
		t.Errorf("a command or hook was registered as a tool: %v", ext.Tools)
	}
}

// TestABadSchemaRefusesTheTool.
//
// Registering it with an empty schema would tell the model the tool takes no
// arguments, so it would be called wrong every time and fail in a way that
// looks like a broken tool rather than a broken manifest.
func TestABadSchemaRefusesTheTool(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{manifests: []plugin.Manifest{
		bundle("acme",
			plugin.Registration{Type: plugin.ToolType, ID: "bad", Parameters: `{"type":`},
			plugin.Registration{Type: plugin.ToolType, ID: "good"},
		),
	}}

	ext := RegisterExtensions(reg, host)
	if _, ok := reg.Get("bad"); ok {
		t.Error("a tool with an unparseable schema was registered")
	}
	if _, ok := reg.Get("good"); !ok {
		t.Error("one bad registration cost a good one in the same manifest")
	}
	if len(ext.Rejected) != 1 || !strings.Contains(ext.Rejected[0], "bad") {
		t.Errorf("rejections = %v, want the bad tool named", ext.Rejected)
	}
}

// TestNameCollisionsStayCallable — two bundles exporting "search" must both
// remain reachable rather than one silently winning, which is how MCP sources
// already behave.
func TestNameCollisionsStayCallable(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{manifests: []plugin.Manifest{
		bundle("alpha", plugin.Registration{Type: plugin.ToolType, ID: "search"}),
		bundle("beta", plugin.Registration{Type: plugin.ToolType, ID: "search"}),
	}}

	ext := RegisterExtensions(reg, host)
	if len(ext.Tools) != 2 {
		t.Fatalf("registered %v, want both", ext.Tools)
	}
	if ext.Tools[0] == ext.Tools[1] {
		t.Fatalf("both registered as %q — one silently replaced the other", ext.Tools[0])
	}
	for _, n := range ext.Tools {
		if _, ok := reg.Get(n); !ok {
			t.Errorf("%s is not callable", n)
		}
	}
}

// TestAnUnknownRegistrationTypeIsReported. Silence would make a typo in a
// manifest indistinguishable from a bundle that declared nothing — the
// silent-no-op failure this codebase keeps hitting.
func TestAnUnknownRegistrationTypeIsReported(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{manifests: []plugin.Manifest{
		bundle("acme", plugin.Registration{Type: "tolo", ID: "typo"}),
	}}
	ext := RegisterExtensions(reg, host)
	if len(ext.Rejected) != 1 || !strings.Contains(ext.Rejected[0], "tolo") {
		t.Errorf("rejections = %v, want the unknown type named", ext.Rejected)
	}
}

// TestAFailingPluginIsAFailedToolNotACrash.
func TestAFailingPluginIsAFailedToolNotACrash(t *testing.T) {
	reg := NewRegistry()
	host := &fakeHost{err: fmt.Errorf("plugin died"), manifests: []plugin.Manifest{
		bundle("acme", plugin.Registration{Type: plugin.ToolType, ID: "flaky"}),
	}}
	RegisterExtensions(reg, host)

	res, err := reg.Execute(context.Background(), "flaky", nil)
	if err != nil {
		t.Fatalf("the registry surfaced an error instead of a failed result: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "plugin died") {
		t.Errorf("result = %+v, want a failure carrying the plugin's error", res)
	}
}

// TestNoBundlesIsNotAnError — the common case.
func TestNoBundlesIsNotAnError(t *testing.T) {
	ext := RegisterExtensions(NewRegistry(), &fakeHost{})
	if len(ext.Tools) != 0 || len(ext.Rejected) != 0 {
		t.Errorf("an empty host produced %+v", ext)
	}
	if got := RegisterExtensions(nil, nil); len(got.Tools) != 0 {
		t.Error("nil arguments were not handled")
	}
}

// TestAPlainStringResultIsUnquoted. The host returns the RPC result verbatim,
// so a plugin answering with a string hands back `"5 words"` — quotes and
// escapes included — and that text goes straight into the model's context.
func TestAPlainStringResultIsUnquoted(t *testing.T) {
	cases := map[string]string{
		`"5 words"`:           "5 words",
		`"a \"quoted\" word"`: `a "quoted" word`,
		`{"count":5}`:         `{"count":5}`, // structure is the answer; untouched
		`[1,2,3]`:             `[1,2,3]`,
		`42`:                  `42`,
		`not json at all`:     `not json at all`,
		`"unterminated`:       `"unterminated`,
	}
	for in, want := range cases {
		reg := NewRegistry()
		host := &fakeHost{reply: in, manifests: []plugin.Manifest{
			bundle("acme", plugin.Registration{Type: plugin.ToolType, ID: "t"}),
		}}
		RegisterExtensions(reg, host)
		res, err := reg.Execute(context.Background(), "t", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Output != want {
			t.Errorf("plugin returned %s → output %q, want %q", in, res.Output, want)
		}
	}
}
