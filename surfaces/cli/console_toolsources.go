package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/infra/config"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/tools/tools"
)

// splitCmd splits a command line into words, honoring single- and
// double-quoted spans (so a quoted argument can contain spaces) and a
// backslash escape for the following character. Its doc comment always
// claimed to honor quoting, but the body was a bare strings.Fields — a
// spaced project name in `/project new "my project" ./path` split into two
// arguments instead of one.
func splitCmd(input string) []string {
	runes := []rune(input)
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune // 0 (unquoted), '\'', or '"'
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			if c == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				continue
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// ---- slash command implementations ----

func (c *Console) printTools() {
	entries := c.registry.List()
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "REGISTERED TOOLS"), paint(cGray, "("+fmtNum(len(entries))+")"))
	for _, e := range entries {
		source := e.Source
		if source == "" {
			source = "builtin"
		}
		fmt.Printf("  %s  %s  %s  %s\n",
			paint(cOrange, padRight(e.Name, 16)),
			paint(cGray, "["+e.Category+"]"),
			paint(cCyan, padRight(source, 14)),
			paint(cGray, strutil.Truncate(e.Description, 40)))
	}
}

// handleTools dispatches /tools subcommands for runtime tool-source management.
//
//	/tools                         list registered tools (built-in + sources)
//	/tools sources                 list tool sources and their connect state
//	/tools connect mcp <name> <cmd> [args...]        spawn a stdio MCP server
//	/tools connect mcp-http <name> <url>             dial an HTTP MCP server
//	/tools connect file <name> <path>                load in-house ITF tools
//	/tools disconnect <id>         disconnect a source (keeps its definition)
//	/tools remove <id>             disconnect + delete a source
func (c *Console) handleTools(args []string) {
	if len(args) == 0 {
		c.printTools()
		return
	}
	switch args[0] {
	case "sources", "source", "src":
		c.printToolSources()
	case "connect", "add":
		c.toolSourceConnect(args[1:])
	case "disconnect":
		c.toolSourceDisconnect(args[1:])
	case "remove", "rm":
		c.toolSourceRemove(args[1:])
	default:
		fmt.Printf("%s unknown /tools subcommand: %s\n", paint(cRed, "✗"), args[0])
		fmt.Printf("  %s /tools [sources|connect|disconnect|remove]\n", paint(cGray, "usage:"))
	}
}

// printToolSources renders the tool-source registry with live status.
func (c *Console) printToolSources() {
	if c.sources == nil {
		fmt.Println(paint(cGray, "  tool source manager not initialized."))
		return
	}
	srcs := c.sources.List()
	if len(srcs) == 0 {
		fmt.Println(paint(cGray, "  no tool sources registered."))
		fmt.Printf("  %s /tools connect mcp <name> <cmd> [args...]   (stdio MCP)\n", paint(cGray, "e.g."))
		fmt.Printf("  %s /tools connect mcp-http <name> <url>       (HTTP MCP)\n", paint(cGray, "     "))
		fmt.Printf("  %s /tools connect file <name> <path>          (in-house ITF)\n", paint(cGray, "     "))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "TOOL SOURCES"), paint(cGray, "("+fmtNum(len(srcs))+")"))
	for _, s := range srcs {
		var statusPaint string
		switch s.Status {
		case "connected":
			statusPaint = paint(cGreen, "● connected")
		case "connecting":
			statusPaint = paint(cYellow, "● connecting")
		case "error":
			statusPaint = paint(cRed, "● error")
		default:
			statusPaint = paint(cGray, "○ disconnected")
		}
		detail := ""
		switch s.Config.Type {
		case "mcp_stdio":
			detail = s.Config.Command + " " + strings.Join(s.Config.Args, " ")
		case "mcp_http":
			detail = s.Config.URL
		case "internal":
			detail = s.Config.Path
		}
		tools := fmtNum(len(s.Tools)) + " tools"
		if s.ServerInfo != "" {
			tools += "  " + s.ServerInfo
		}
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			paint(cWhite, padRight(s.Config.ID, 22)),
			paint(cBlue, padRight(string(s.Config.Type), 10)),
			statusPaint,
			paint(cGray, padRight(tools, 26)),
			paint(cGray, strutil.Truncate(detail, 40)))
		if s.Error != "" {
			fmt.Printf("     %s %s\n", paint(cRed, "last error:"), paint(cGray, strutil.Truncate(s.Error, 70)))
		}
	}
	fmt.Printf("\n  %s /tools connect mcp <name> <cmd> [args...]   ·   /tools disconnect <id>\n", paint(cGray, "add:"))
}

// toolSourceConnect parses a connect command and adds + connects a source.
func (c *Console) toolSourceConnect(args []string) {
	if c.sources == nil {
		fmt.Println(paint(cRed, "✗ tool source manager not initialized"))
		return
	}
	if len(args) < 2 {
		fmt.Printf("%s usage:\n", paint(cRed, "✗"))
		fmt.Printf("  %s /tools connect mcp <name> <cmd> [args...]   (stdio MCP server)\n", paint(cGray, ""))
		fmt.Printf("  %s /tools connect mcp-http <name> <url>        (HTTP MCP server)\n", paint(cGray, ""))
		fmt.Printf("  %s /tools connect file <name> <path>           (in-house ITF tools)\n", paint(cGray, ""))
		fmt.Printf("  %s /tools connect htp <name> <url>             (remote HTP device)\n", paint(cGray, ""))
		return
	}
	kind := args[0]
	name := args[1]
	var cfg tools.SourceConfig
	cfg.Name = name
	cfg.AutoConnect = true // remember to reconnect on next launch
	switch kind {
	case "mcp":
		if len(args) < 3 {
			fmt.Printf("%s /tools connect mcp <name> <cmd> [args...]\n", paint(cRed, "✗ missing command"))
			return
		}
		cfg.Type = tools.SourceMCPStdio
		cfg.Command = args[2]
		cfg.Args = args[3:]
	case "mcp-http", "mcp_http", "http":
		if len(args) < 3 {
			fmt.Printf("%s /tools connect mcp-http <name> <url>\n", paint(cRed, "✗ missing url"))
			return
		}
		cfg.Type = tools.SourceMCPHTTP
		cfg.URL = args[2]
	case "file", "internal", "itf":
		if len(args) < 3 {
			fmt.Printf("%s /tools connect file <name> <path>\n", paint(cRed, "✗ missing path"))
			return
		}
		cfg.Type = tools.SourceInternal
		cfg.Path = args[2]
	case "htp":
		// Connect to a REMOTE DarkCode Tool Protocol server (an outer/remote
		// device). The server's tools are auto-discovered and registered.
		if len(args) < 3 {
			fmt.Printf("%s /tools connect htp <name> <url>\n", paint(cRed, "✗ missing url"))
			return
		}
		cfg.Type = tools.SourceHTP
		cfg.URL = args[2]
	default:
		fmt.Printf("%s unknown source kind %s (mcp | mcp-http | file | htp)\n", paint(cRed, "✗"), kind)
		return
	}

	id, err := c.sources.Add(cfg)
	if err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := c.sources.Connect(ctx, id); err != nil {
		fmt.Printf("%s added %s but failed to connect: %s\n", paint(cYellow, "⚠"), paint(cWhite, name), err)
		return
	}
	src, _ := c.sources.Get(id)
	c.persistSources()
	fmt.Printf("%s connected %s  %s  %s  %s\n",
		paint(cGreen, "✓"),
		paint(cWhite+clrBold, name),
		paint(cBlue, "("+string(cfg.Type)+")"),
		paint(cGray, fmtNum(len(src.Tools))+" tools"),
		paint(cGray, "(saved to .config)"))
}

// toolSourceDisconnect disconnects a source by id (keeps the definition).
func (c *Console) toolSourceDisconnect(args []string) {
	if c.sources == nil {
		fmt.Println(paint(cRed, "✗ tool source manager not initialized"))
		return
	}
	if len(args) < 1 {
		fmt.Printf("%s usage: /tools disconnect <id>\n", paint(cRed, "✗"))
		return
	}
	id := args[0]
	if err := c.sources.Disconnect(id); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.persistSources()
	fmt.Printf("%s disconnected %s %s\n", paint(cGreen, "✓"), paint(cWhite, id), paint(cGray, "(tools removed; definition retained)"))
}

// toolSourceRemove disconnects and deletes a source by id.
func (c *Console) toolSourceRemove(args []string) {
	if c.sources == nil {
		fmt.Println(paint(cRed, "✗ tool source manager not initialized"))
		return
	}
	if len(args) < 1 {
		fmt.Printf("%s usage: /tools remove <id>\n", paint(cRed, "✗"))
		return
	}
	id := args[0]
	if err := c.sources.Remove(id); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.persistSources()
	fmt.Printf("%s removed %s %s\n", paint(cGreen, "✓"), paint(cWhite, id), paint(cGray, "(saved to .config)"))
}

// persistSources mirrors the server's persistSources: writes the current
// source set back into .config so changes survive restarts.
func (c *Console) persistSources() {
	if c.sources == nil || c.cfg == nil {
		return
	}
	cfgs := c.sources.Configs()
	out := make([]config.ToolSourceConfig, 0, len(cfgs))
	for _, sc := range cfgs {
		out = append(out, config.ToolSourceConfig{
			ID:          sc.ID,
			Name:        sc.Name,
			Type:        string(sc.Type),
			Command:     sc.Command,
			Args:        sc.Args,
			Env:         sc.Env,
			URL:         sc.URL,
			Headers:     sc.Headers,
			Path:        sc.Path,
			AutoConnect: sc.AutoConnect,
		})
	}
	c.cfg.ToolSources = out
	_ = c.cfg.Save()
}
