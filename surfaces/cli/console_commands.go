package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/darkcode/kernel/verb"
	"github.com/darkcode/memory/ingest"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/model/provider/embedded"
	"github.com/darkcode/surfaces/cli/tui"
	"github.com/darkcode/tools/tools"
)

// ---- slash command dispatch ----

// handleSlash processes a slash command. Returns true if the console should exit.
func (c *Console) handleSlash(input string) bool {
	parts := splitCmd(input)
	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit", "/q":
		fmt.Println(paint(cGray, " goodbye."))
		return true

	case "/help", "/?":
		if sel := tui.Select("Commands — type to search, Enter to run:", commandSelectorItems()); sel != "" {
			return c.handleSlash(sel)
		}

	case "/monitor":
		c.runDashboard()

	case "/usage":
		c.printUsageFull()

	case "/history":
		c.printHistoryFull()

	case "/stats":
		c.printHardwareStats()

	case "/status":
		fmt.Println(c.kernel.Status())

	case "/tools":
		c.handleTools(parts[1:])

	case "/plugins":
		fmt.Println(paint(cBlue, "📦 Enterprise Plugin System"))
		fmt.Println(paint(cGray, "   (Connected via gRPC to dynamic loader)"))

	case "/pipeline":
		fmt.Println(paint(cGreen+clrBold, "✔️ VERIFICATION PIPELINE"))
		fmt.Println(paint(cGray, "   1. ") + paint(cWhite, "Syntax Check ") + paint(cGray, "(Deterministic Tree-sitter)"))
		fmt.Println(paint(cGray, "   2. ") + paint(cWhite, "Format Check ") + paint(cGray, "(gofmt/prettier)"))
		fmt.Println(paint(cGray, "   3. ") + paint(cWhite, "Linting      ") + paint(cGray, "(golangci-lint/eslint)"))
		fmt.Println(paint(cGray, "   4. ") + paint(cWhite, "Type Check   ") + paint(cGray, "(go build/tsc)"))
		fmt.Println(paint(cGray, "   5. ") + paint(cWhite, "Test Check   ") + paint(cGray, "(go test/jest)"))
		fmt.Println(paint(cGray, "   [Status: Online & Enforcing]"))

	case "/memory":
		fmt.Println(c.mem.Summary())

	case "/ingest":
		src := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), cmd))
		if src == "" {
			fmt.Println(paint(cYellow, "usage: /ingest <file | directory | http(s) url | text>"))
			break
		}
		fmt.Println(paint(cGray, "  ingesting…"))
		st, err := ingest.New(c.mem, c.mem.KG()).Ingest(context.Background(), src)
		if err != nil {
			fmt.Println(paint(cRed, "  ✗ "+err.Error()))
			break
		}
		msg := fmt.Sprintf("  ✓ ingested %d source(s) → %d memory chunk(s)", st.Sources, st.Chunks)
		if st.KGNodes > 0 {
			msg += fmt.Sprintf(", %d code nodes", st.KGNodes)
		}
		if st.Skipped > 0 {
			msg += fmt.Sprintf(", %d skipped", st.Skipped)
		}
		fmt.Println(paint(cGreen, msg))

	case "/rollback", "/undo":
		c.handleRollback(parts[1:])

	case "/health":
		c.handleHealth()

	case "/evolution":
		c.handleEvolution(parts[1:])

	case "/session", "/sessions":
		c.handleSession(parts[1:])

	case "/episodes":
		c.printEpisodes()

	case "/skills":
		// Import written-down procedure into procedural memory. Without a path
		// this lists what is already stored, since "what do I know?" is asked
		// more often than "learn this".
		if len(parts) > 2 && parts[1] == "import" {
			found, err := c.mem.ImportSkills(parts[2])
			if err != nil {
				fmt.Println("import failed:", err)
				break
			}
			fmt.Print(memory.FormatImport(found))
			break
		}
		c.printSkills()

	case "/know", "/knowledge":
		if len(parts) > 1 {
			c.printConceptRelations(parts[1])
		} else {
			c.printKnowledge()
		}

	case "/new", "/reset":
		// StartNewSession clears STM and advances the session epoch so prior
		// conversations stop resurfacing via episodic recall (durable memory
		// is kept). Matches the GUI "New Chat" behavior.
		c.mem.StartNewSession()
		if c.gate != nil {
			c.gate.ResetSession()
		}
		c.activityMu.Lock()
		c.activity = c.activity[:0]
		c.activityMu.Unlock()
		if c.recorder != nil {
			c.recorder.Clear()
		}
		fmt.Println(paint(cGreen, "✓") + paint(cGray, " fresh session started — prior chat context cleared."))

	case "/config":
		c.printConfig()

	case "/model":
		if len(parts) > 1 {
			c.setModel(parts[1])
		} else {
			var items []tui.SelectorItem
			for k, m := range c.cfg.Models {
				desc := fmt.Sprintf("%s [%s]", m.Provider, m.Tier)
				if k == c.cfg.Model {
					desc += " (current)"
				}
				items = append(items, tui.SelectorItem{Title: k, Description: desc, Value: k})
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Title < items[j].Title })

			selected := tui.Select("Select Model to switch to:", items)
			if selected != "" {
				c.setModel(selected)
			}
		}

	case "/mode":
		if len(parts) > 1 {
			c.setMode(parts[1])
		} else {
			mode := tui.Select("Select Routing Mode:", []tui.SelectorItem{
				{Title: "single", Description: "Single model execution (fastest)", Value: "single"},
				{Title: "escalation", Description: "Escalates to smarter models if needed", Value: "escalation"},
				{Title: "consensus", Description: "Multi-model consensus (most reliable)", Value: "consensus"},
			})
			if mode != "" {
				c.setMode(mode)
			}
		}

	// A bare verb with no task: explain it rather than arm it. Silently
	// switching into a mode because someone typed a word is the sticky-mode
	// trap the verbs exist to avoid.
	case "/ask", "/loop", "/graph":
		fmt.Print(verbHelp())

	// /always makes a verb sticky. Deliberately NOT /mode — that is already
	// the routing mode (single/escalation/consensus), and overloading it would
	// recreate the ambiguity these verbs exist to remove.
	case "/always":
		if len(parts) > 1 {
			c.setAlways(parts[1])
			break
		}
		cur := c.stickyVerb
		if cur == "" {
			cur = "off — chosen per message"
		} else {
			cur = "/" + cur
		}
		items := []tui.SelectorItem{{
			Title:       "off",
			Description: "Let escalation choose per message, and climb when it needs to",
			Value:       "off",
		}}
		for _, n := range verb.Names() {
			st, _ := verb.Lookup(n)
			items = append(items, tui.SelectorItem{Title: "/" + n, Description: st.Help, Value: n})
		}
		if m := tui.Select("Always use (current: "+cur+"):", items); m != "" {
			c.setAlways(m)
		}

	case "/background":
		if len(parts) > 1 {
			c.setBackgroundWork(parts[1])
			break
		}
		cur := c.cfg.ResolvedBackgroundWork()
		if lv := tui.Select("Background work (current: "+cur+"):", []tui.SelectorItem{
			{Title: "off", Description: "Nothing runs on its own", Value: "off"},
			{Title: "light", Description: "Keep the workspace index current", Value: "light"},
			{Title: "full", Description: "Indexing plus the repo-health daemon", Value: "full"},
		}); lv != "" {
			c.setBackgroundWork(lv)
		}

	case "/brain":
		if len(parts) > 1 {
			c.setBrain(parts[1])
		} else {
			b := tui.Select("Select Brain (current: "+c.brain+"):", []tui.SelectorItem{
				{Title: "auto", Description: "Local-first, escalate to cloud for hard tasks", Value: "auto"},
				{Title: "local", Description: "Offline — always use the local model", Value: "local"},
				{Title: "cloud", Description: "Prefer cloud models", Value: "cloud"},
			})
			if b != "" {
				c.setBrain(b)
			}
		}

	case "/memory-profile", "/memprofile":
		if len(parts) > 1 {
			c.setMemoryProfile(parts[1])
		} else {
			cur := c.cfg.MemoryProfile
			if cur == "" {
				cur = "auto"
			}
			p := tui.Select("Select Memory Profile (current: "+cur+"):", []tui.SelectorItem{
				{Title: "lean", Description: "8K context — lowest RAM, fine for chat + small coding", Value: "lean"},
				{Title: "balanced", Description: "16K context — comfortable for RAG + a project brief (default)", Value: "balanced"},
				{Title: "max", Description: "32K context — largest window, highest RAM", Value: "max"},
				{Title: "auto", Description: "Let the resource governor decide", Value: "auto"},
			})
			if p != "" {
				c.setMemoryProfile(p)
			}
		}

	case "/profile":
		if len(parts) > 1 {
			c.setProfile(parts[1])
		} else {
			prof := tui.Select("Select Execution Profile:", []tui.SelectorItem{
				{Title: "auto", Description: "Smart: sequential for free-tier, parallel otherwise", Value: "auto"},
				{Title: "sequential", Description: "One model call at a time (429-safe for free-tier)", Value: "sequential"},
				{Title: "parallel", Description: "DAG sub-agents + consensus run concurrently", Value: "parallel"},
			})
			if prof != "" {
				c.setProfile(prof)
			}
		}

	case "/local":
		if len(parts) > 1 {
			c.setLocal(parts[1:])
		} else {
			resolved := c.cfg.ResolvedLocalMode()
			status := paint(cRed, "off")
			switch resolved {
			case "force":
				status = paint(cGreen+clrBold, "force")
			case "off":
				status = paint(cRed, "off")
			default:
				if c.cfg.LocalEnabled() {
					status = paint(cGreen, "on")
				}
			}
			offloadStatus := paint(cRed, "off")
			if c.cfg.EnableLocalOffloading {
				offloadStatus = paint(cGreen, "on")
			}
			fmt.Printf("%s %s (%s)  %s %s\n",
				paint(cGray, "local llm:"), status,
				resolved,
				paint(cGray, "offloading:"), offloadStatus,
			)
			if resolved == "force" {
				fmt.Printf("  %s\n", paint(cGray, "routing pinned to local — cloud providers will not be used"))
			}
			// When the resource governor refused local, say WHY — "local is
			// mysteriously off" is exactly the silent degradation the
			// never-force design forbids.
			if reason := embedded.Default().LoadRefusal(); reason != "" {
				fmt.Printf("%s %s\n", paint(cYellow, "⚠"), paint(cGray, reason))
			}
		}

	case "/safety":
		if len(parts) > 1 {
			c.setSafety(parts[1])
		} else {
			safe := tui.Select("Select Safety Level:", []tui.SelectorItem{
				{Title: "strict", Description: "Ask before ANY system action", Value: "strict"},
				{Title: "normal", Description: "Ask before dangerous actions", Value: "normal"},
				{Title: "relaxed", Description: "Auto-approve most actions", Value: "relaxed"},
			})
			if safe != "" {
				c.setSafety(safe)
			}
		}

	case "/sandbox":
		if len(parts) > 1 {
			c.setSandbox(parts[1])
		} else {
			c.printSandboxStatus()
			m := tui.Select("Select Sandbox Mode:", []tui.SelectorItem{
				{Title: "off", Description: "Never confine shell commands", Value: "off"},
				{Title: "auto", Description: "Confine when bwrap/firejail is installed", Value: "auto"},
				{Title: "on", Description: "Confine; warn but run if no backend", Value: "on"},
				{Title: "strict", Description: "Require confinement; refuse without a backend", Value: "strict"},
			})
			if m != "" {
				c.setSandbox(m)
			}
		}

	case "/compressor":
		if len(parts) > 1 {
			c.setCompressor(parts[1])
		} else {
			name := c.cfg.CompressorModel
			if name == "" {
				name = "<primary model>"
			}
			fmt.Printf("%s %s\n", paint(cGray, "compressor model:"), paint(cOrange+clrBold, name))
		}

	case "/models":
		c.handleModels(parts[1:])

	case "/providers":
		c.handleProviders(parts[1:])

	case "/events":
		c.streamEv = !c.streamEv
		state := paint(cGreen, "ON")
		if !c.streamEv {
			state = paint(cRed, "OFF")
		}
		fmt.Printf("%s progress indicator is %s %s\n", paint(cGray, "✓"), state, paint(cGray, "(intermediate events are always in /log)"))

	case "/log":
		c.printActivityLog()

	case "/project", "/projects":
		c.handleProjects(parts[1:])

	case "/audit":
		c.printAudit()

	case "/learning":
		c.printLearningStats()

	case "/plan":
		if c.activeProject == "" {
			fmt.Println(paint(cGray, "  no active project. activate one with /project <id>"))
		} else {
			plan, _ := c.projects.GetPlan(c.activeProject)
			if plan == "" {
				fmt.Println(paint(cGray, "  no implementation plan found for the active project."))
			} else {
				fmt.Printf("%s\n%s\n", paint(cAmber+clrBold, "IMPLEMENTATION PLAN"), paint(cWhite, plan))
			}
		}

	case "/workflow":
		if c.activeProject == "" {
			fmt.Println(paint(cGray, "  no active project. activate one with /project <id>"))
		} else {
			workflow, _ := c.projects.GetWorkflow(c.activeProject)
			if workflow == "" {
				fmt.Println(paint(cGray, "  no workflow architecture found for the active project."))
			} else {
				fmt.Printf("%s\n%s\n", paint(cAmber+clrBold, "WORKFLOW ARCHITECTURE"), paint(cWhite, workflow))
			}
		}

	case "/permissions", "/perms":
		c.printPermissions(parts[1:])

	case "/lock-tests":
		c.handleLockTests(parts[1:])

	default:
		// An extension bundle may own this name. Checked here rather than in the
		// switch above because built-ins win: a bundle must not be able to
		// shadow /permissions or /rollback by choosing the name.
		if c.runExtensionCommand(cmd, strings.Join(parts[1:], " ")) {
			return false
		}
		fmt.Printf("%s unknown command: %s %s\n", paint(cRed, "✗"), cmd, paint(cGray, "(try /help)"))
	}

	return false
}

// SetExtensionCommands installs the slash commands loaded bundles offer.
//
// A setter rather than an eleventh constructor parameter: the console already
// takes ten, and a bundle's commands are not needed to build one.
func (c *Console) SetExtensionCommands(cmds []tools.ExtensionCommand) {
	c.extCommands = cmds
}

// runExtensionCommand executes a bundle's slash command, reporting whether it
// handled the name at all.
//
// The rest of the line is passed as {"input": "..."} — a bundle wanting real
// arguments declares a tool schema and lets the agent call it, which is the
// path that gets validation, the permission gate and the hooks. A slash command
// is the convenience shortcut, not a second calling convention.
func (c *Console) runExtensionCommand(cmd, rest string) bool {
	name := strings.TrimPrefix(cmd, "/")
	for _, ec := range c.extCommands {
		if ec.Name != name {
			continue
		}
		args := map[string]interface{}{}
		if strings.TrimSpace(rest) != "" {
			args["input"] = strings.TrimSpace(rest)
		}
		// Through the registry, so an extension command is gated, recorded and
		// hooked exactly like any other tool call.
		res, err := c.registry.Execute(context.Background(), ec.Tool, args)
		switch {
		case err != nil:
			fmt.Printf("%s %s: %v\n", paint(cRed, "✗"), name, err)
		case res != nil && !res.Success:
			fmt.Printf("%s %s: %s\n", paint(cRed, "✗"), name, res.Error)
		case res != nil:
			fmt.Println(res.Output)
		}
		return true
	}
	return false
}
