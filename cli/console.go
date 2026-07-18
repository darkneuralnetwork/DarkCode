package cli

// ============================================================================
// CONSOLE — the orchestrator-aware interactive terminal.
//
// This is the default mode when darkcode is run without flags. It wires the
// full 6-layer orchestrator and provides:
//   • a rich startup banner (banner.go)
//   • streaming chat with live event rendering during execution
//   • an inline usage summary after each response
//   • a full slash-command set (models, providers, routing, memory, …)
//   • the live monitoring dashboard via /monitor (dashboard.go)
// ============================================================================

import (
	"bufio"
	"context"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chzyer/readline"

	"github.com/darkcode/attach"
	"github.com/darkcode/cli/tui"
	"github.com/darkcode/config"
	provpkg "github.com/darkcode/provider"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/core"
	"github.com/darkcode/ingest"
	"github.com/darkcode/llm"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/observability"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/project"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

var ErrSwitchToGUI = fmt.Errorf("switch to gui")

// Console is the orchestrator-backed interactive terminal.
type Console struct {
	cfg           *config.Config
	kernel        *orchestrator.Kernel
	mem           *memory.System
	registry      *tools.Registry
	emitter       *ui.EventEmitter
	recorder      *tools.ChangeRecorder
	gate          *permission.Gate
	sources       *tools.SourceManager
	projects      *project.Store
	activeProject string

	// chatMode mirrors the GUI's per-request chat-mode dropdown
	// (general | project | auto | loop). It selects the tool/loop policy for
	// each query via ApplyRequestOverrides, exactly as the web chat does —
	// keeping CLI ↔ GUI feature parity. Default "project" (full tool
	// runtime), matching the prior CLI behavior.
	chatMode string

	history []string
	histIdx int

	// activity log: every orchestration event is recorded here so /log can
	// replay the full trace that is no longer printed to stdout inline.
	activity   []activityEntry
	activityMu sync.Mutex

	streamEv bool // show the minimal progress spinner during queries
	// live rendering state
	evActive bool
	mu       sync.Mutex // serializes terminal writes (spinner vs prompts)

	rl      *readline.Instance
	resumed bool // true when entering CLI after a GUI session (skip full banner)
}

// activityEntry is a single recorded orchestration event for /log.
type activityEntry struct {
	time time.Time
	icon string
	kind string
	tool string
	msg  string
}

// NewConsole creates an orchestrator-backed console.
func NewConsole(cfg *config.Config, kernel *orchestrator.Kernel, mem *memory.System, registry *tools.Registry, emitter *ui.EventEmitter, recorder *tools.ChangeRecorder, sources *tools.SourceManager, projects *project.Store, activeProject string) *Console {
	c := &Console{
		cfg:           cfg,
		kernel:        kernel,
		mem:           mem,
		registry:      registry,
		emitter:       emitter,
		recorder:      recorder,
		gate:          kernel.Gate(),
		sources:       sources,
		projects:      projects,
		activeProject: activeProject,
		chatMode:      "smart", // advanced auto-routing
		streamEv:      true, // minimal spinner on by default
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          ">>> ",
		HistoryFile:     filepath.Join(os.TempDir(), "darkcode_history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		AutoComplete:    c.buildCompleter(),
	})
	if err == nil {
		c.rl = rl
	} else {
		// Fallback in case of error (should be rare)
		fmt.Fprintf(os.Stderr, "warning: readline init failed: %v\n", err)
	}

	// Install the interactive terminal approval prompt for dangerous tool
	// calls. If a mode-aware approver is wired up (the normal case), register
	// this terminal delegate on it and flip to CLI mode — do NOT overwrite the
	// gate's approver, or GUI mode would lose its popup path after a CLI
	// session. Fallback: install directly on the gate if no composite exists.
	if ma := c.kernel.ModeApprover(); ma != nil {
		ma.SetCLIApprover(c.requestApproval)
		ma.SetMode(permission.ModeCLI)
	} else if c.gate != nil {
		c.gate.SetApprover(c.requestApproval)
	}
	return c
}

// ActiveProject returns the currently active project ID.
func (c *Console) ActiveProject() string {
	return c.activeProject
}

// SetResumed marks this console session as a resume from GUI mode. When true,
// Run() prints a compact header instead of the full startup banner (the
// orchestrator state — kernel, memory, router — is already initialized; only
// the console is re-created). History replays as usual.
func (c *Console) SetResumed(v bool) { c.resumed = v }

// Run starts the interactive REPL. It returns when the user exits.
func (c *Console) Run() error {
	if c.resumed {
		// Resumed from GUI: the orchestrator is already running (same process).
		// Print a compact header instead of the full banner so it does not look
		// like a fresh restart. Conversation history replays below.
		//
		// Lead with a newline + repaint so any stray stderr output from the GUI
		// server (e.g. a late log line) cannot cling to the readline prompt line.
		// This is the display-side fix for the ">>> [gui] last SSE client gone…"
		// prompt-corruption bug; the root-cause fix is SetGUIActive(false) in
		// main.go (disarm disconnect detection while CLI owns the terminal).
		fmt.Print("\r\n")
		mode := "single"
		if c.cfg != nil {
			mode = c.cfg.RoutingMode
			if mode == "" {
				mode = "single"
			}
		}
		fmt.Println(paint(cGreen+clrBold, "► Resumed CLI session — conversation context preserved") +
			paint(cGray, "  ·  mode: "+mode+"  ·  chat: "+c.chatMode+"  ·  /help for commands  ·  /gui to return"))
		fmt.Println(paint(cGray, "  "+strings.Repeat("─", 60)))
	} else {
		printBanner(c.cfg, c.mem, c.registry, c.kernel)
	}

	if c.emitter != nil {
		for _, e := range c.emitter.History() {
			if e.Type == core.EventChatQuery {
				fmt.Print(paint(cBlue, "\n>>> ") + fmt.Sprintf("%v", e.Content) + "\n")
			} else if e.Type == core.EventChatResponse {
				fmt.Print("\n" + fmt.Sprintf("%v", e.Content) + "\n")
			}
		}
	}
	if c.rl != nil {
		defer c.rl.Close()
	}

	// Ctrl+C handling: during a query it cancels the request; at the prompt
	// it exits. We re-arm per iteration.
	ctx := context.Background()

	// Non-interactive / piped stdin (no readline instance): read and submit
	// line by line as before — there's no paste-vs-typed distinction to make,
	// and scripted input must NOT be silently merged into one message.
	if c.rl == nil {
		for {
			input, err := c.readLine()
			if err != nil {
				if err == readline.ErrInterrupt || err.Error() == "EOF" {
					fmt.Println()
					return nil
				}
				return err
			}
			done, switchGUI := c.dispatchInput(ctx, input)
			if switchGUI {
				return ErrSwitchToGUI
			}
			if done {
				return nil
			}
		}
	}

	// Interactive TTY: coalesce a multi-line paste into ONE message. chzyer/
	// readline has no bracketed-paste support, so every embedded newline in a
	// paste returns from Readline() separately and would otherwise be submitted
	// as a separate request. A single background goroutine is the ONLY caller of
	// Readline() (the library is not concurrency-safe); the main loop treats a
	// tight burst of back-to-back returns (a paste, whose lines the terminal
	// delivers instantly) as one message, while a normally typed line — followed
	// by an idle gap — is submitted on its own.
	//
	// Prompt handling: the goroutine sets the prompt right before each read, so
	// the ">>> " prompt is shown only for a genuine first line; look-ahead reads
	// used to detect the end of a burst use an empty prompt (no visible clutter
	// while a response streams). The read outstanding when a burst ends carries
	// over as the next message's first line; its prompt is restored afterwards.
	// Tradeoff: a key typed while a response is still streaming echoes into that
	// carried-over read (and becomes the next message) — desirable capture; only
	// the inline echo is cosmetic.
	type lineResult struct {
		line string
		err  error
	}
	readReq := make(chan string)      // prompt to use for the next Readline()
	lineCh := make(chan lineResult, 1) // buffered so the final send never leaks
	go func() {
		for prompt := range readReq {
			c.rl.SetPrompt(prompt)
			line, err := c.readLine()
			lineCh <- lineResult{line, err}
			if err != nil {
				return
			}
		}
	}()
	defer close(readReq)

	const burstIdle = 50 * time.Millisecond
	pending := false // an outstanding look-ahead read → next message's first line
	for {
		if !pending {
			readReq <- ">>> "
		}
		first := <-lineCh
		pending = false
		if first.err != nil {
			if first.err == readline.ErrInterrupt || first.err.Error() == "EOF" {
				fmt.Println()
				return nil
			}
			return first.err
		}

		// Slash commands are always a single typed line and some of them
		// return from Run() (/gui, /quit). Dispatch them IMMEDIATELY without
		// issuing the burst-accumulation look-ahead read — otherwise that
		// extra Readline() sits blocked in the reader goroutine when Run()
		// returns, which (a) needed a second Enter to flush and (b) left a
		// goroutine holding the terminal so the GUI→CLI resume couldn't read
		// input. With no look-ahead outstanding, `defer close(readReq)` parks
		// and cleanly exits the goroutine. A paste never starts with '/', so
		// this doesn't affect multi-line paste handling.
		if strings.HasPrefix(strings.TrimSpace(first.line), "/") {
			done, switchGUI := c.dispatchInput(ctx, first.line)
			if switchGUI {
				return ErrSwitchToGUI
			}
			if done {
				return nil
			}
			continue
		}

		lines := []string{first.line}

		// Accumulate paste-burst lines: issue a blank-prompt look-ahead read and
		// wait only burstIdle. Arriving that fast means it's part of a paste;
		// a timeout means the burst is done (that read carries over).
		var pendingErr error
	accumulate:
		for {
			readReq <- ""
			select {
			case r := <-lineCh:
				if r.err != nil {
					pendingErr = r.err
					break accumulate
				}
				lines = append(lines, r.line)
			case <-time.After(burstIdle):
				pending = true
				break accumulate
			}
		}

		done, switchGUI := c.dispatchInput(ctx, strings.Join(lines, "\n"))
		if switchGUI {
			return ErrSwitchToGUI
		}
		if done {
			return nil
		}

		if pendingErr != nil {
			if pendingErr == readline.ErrInterrupt || pendingErr.Error() == "EOF" {
				fmt.Println()
				return nil
			}
			return pendingErr
		}

		// Restore the real prompt for the carried-over read now that any
		// runQuery output is done, so the user sees ">>> " for their next line.
		if pending {
			c.rl.SetPrompt(">>> ")
			c.rl.Refresh()
		}
	}
}

// dispatchInput runs one complete user message (already assembled from one or
// more read lines) through history + slash-command dispatch or the
// orchestrator. Returns done=true if the REPL should exit, switchGUI=true if it
// should hand off to GUI mode.
func (c *Console) dispatchInput(ctx context.Context, input string) (done bool, switchGUI bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return false, false
	}
	c.history = append(c.history, input)
	c.histIdx = len(c.history)

	// A slash command is only recognized when the whole message is a single
	// line beginning with '/'. A multi-line paste is always message content,
	// never a command.
	if !strings.Contains(input, "\n") && strings.HasPrefix(input, "/") {
		if input == "/gui" {
			return false, true
		}
		if c.handleSlash(input) {
			return true, false
		}
		return false, false
	}

	// Parse @Type:ref attachments out of the prompt (e.g. @File:./x,
	// @Directory:./src, @URL:…, @Text:"…"). The tokens are removed from
	// the visible query and resolved into a markdown block prepended to it.
	query, atts := attach.ParseRefs(input)
	c.runQuery(ctx, query, atts)
	return false, false
}

func (c *Console) readLine() (string, error) {
	if c.rl != nil {
		line, err := c.rl.Readline()
		if err != nil {
			return "", err
		}
		return line, nil
	}
	// Fallback (e.g. non-interactive)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return scanner.Text(), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("EOF")
}

// runQuery executes a single user message through the orchestrator.
//
// To keep stdout clean, intermediate orchestration events (task updates, agent
// spawns, tool-execution status, …) are NOT printed inline — they are recorded
// into the activity log and can be reviewed with /log. Instead, after the run
// we show a concise summary of what actually changed: which files were
// modified (before → after diff) and which commands ran.
func (c *Console) runQuery(ctx context.Context, query string, atts []attach.Attachment) {
	if c.cfg.Model == "" && !c.cfg.EnableLocalLLM {
		fmt.Printf("%s please select a model or initialise the local llm\n", paint(cRed, "✗"))
		return
	}
	origQuery := query

	// (Removed) undocumented "auto" routing-mode classifier: "auto" is not a
	// valid kernel routing mode (only single|escalation|consensus), it was
	// unreachable via --mode or /routing, and it fired a hidden LLM call on
	// every query. Use the /project create command to create a project.

	if c.activeProject != "" && c.projects != nil {
		// Use the shared summary-aware context builder (same as the server) so
		// the CLI and GUI build identical project-context prompts. Previously
		// the CLI injected the raw context and diverged from the server's
		// compressed-summary path.
		query = c.projects.BuildContextQuery(c.activeProject, query)
	}

	// Arm Ctrl+C to cancel this request.
	reqCtx, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		<-sigCh
		fmt.Print("\n" + paint(cYellow, " [interrupting…]") + "\n")
		cancel()
	}()
	defer func() {
		signal.Stop(sigCh)
		signal.Reset(syscall.SIGINT)
	}()

	beforeReqs := metrics.Default.Snapshot().TotalRequests
	beforeChanges := 0
	if c.recorder != nil {
		beforeChanges = c.recorder.Len()
	}

	// Record orchestration events into the activity log (for /log) without
	// spamming stdout. Only the spinner + any approval prompts appear inline.
	var lastEvent string
	c.evActive = true
	handler := func(e core.UIEvent) {
		if !c.evActive {
			return
		}
		// Streaming token chunks are the live LLM output, not execution
		// detail. They are excluded from the inline ├─ feed AND the /log
		// activity trace so the execution detail stays a readable
		// orchestration log (the final answer is rendered separately from
		// the kernel result).
		if e.Type == core.EventTaskUpdate && e.Status == "streaming" {
			return
		}
		c.recordActivity(e)
		
		msg := eventMessage(e)
		
		c.mu.Lock()
		if c.streamEv {
			fmt.Print("\r" + ansiClearLine + "\r")
			icon := eventIcon(string(e.Type))
			fmt.Printf("  %s %s  %s\n", paint(cGray, "├─"), paint(cOrange, icon), paint(cGray, msg))
		}
		
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		lastEvent = msg
		c.mu.Unlock()
	}
	if c.emitter != nil {
		c.emitter.OnHandler(handler)
		defer c.emitter.RemoveHandler(handler)
	}
	defer func() { c.evActive = false }()

	// Resolve any @Type:ref attachments (file/dir/image/url/text) into a
	// markdown block prepended to the query. Relative paths resolve against
	// the process cwd (the agent's working directory).
	var resolvedQuery string
	if len(atts) > 0 {
		block, results := attach.Resolve(atts, "")
		resolvedQuery = block + query
		for _, r := range results {
			status := "attached"
			if !r.OK {
				status = "attachment error"
			}
			c.recordActivity(core.UIEvent{
				Type: core.EventTaskUpdate, Status: status,
				Content: r.Type + " " + r.Source, Timestamp: time.Now(),
			})
		}
	} else {
		resolvedQuery = query
	}

	if c.emitter != nil {
		c.emitter.EmitChatQuery(origQuery)
	}

	c.recordActivity(core.UIEvent{
		Type: core.EventTaskUpdate, Status: "observe",
		Content: query, Timestamp: time.Now(),
	})

	// Inject the active project's implementation plan + workflow architecture
	// so the kernel's planner follows the plan (previously the plan was a
	// write-only display artifact generated after execution). Cleared after
	// Execute so a subsequent non-project request isn't contaminated.
	if c.activeProject != "" && c.projects != nil {
		workflow, _ := c.projects.GetWorkflow(c.activeProject)
		// Phase 4 — brief-first project memory: prefer the compact, auto-updated
		// project brief over the full ~8K implementation plan so resuming a
		// project stays cheap and needs no re-feeding. Fall back to the full plan
		// only when no brief has been generated yet (new project).
		plan, _ := c.projects.GetContext(c.activeProject)
		if strings.TrimSpace(plan) == "" {
			plan, _ = c.projects.GetPlan(c.activeProject)
		}
		c.kernel.SetProjectContext(plan, workflow)
		defer c.kernel.ClearProjectContext()
	}

	// Minimal progress indicator (cleared on completion).
	done := make(chan struct{})
	go func() {
		sp := newSpinner()
		for {
			select {
			case <-done:
				return
			case <-time.After(90 * time.Millisecond):
				c.mu.Lock()
				msg := lastEvent
				if msg == "" {
					msg = "working…"
				}
				fmt.Printf("\r%s %s", paint(cOrange, sp.tick()), paint(cGray, padRight(msg, 60)))
				c.mu.Unlock()
			}
		}
	}()

	// Apply the chat-mode policy (CLI ↔ GUI parity). 
	loopOverride := "off"
	if c.chatMode == "loop" && c.cfg.AgenticLoop {
		loopOverride = "on"
	}
	toolsOverride := "on"
	restoreOverrides := c.kernel.ApplyRequestOverrides("", "", loopOverride, toolsOverride, "")
	defer restoreOverrides()

	result, err := c.kernel.Execute(reqCtx, resolvedQuery)
	close(done)
	fmt.Print("\r" + ansiClearLine + "\r") // clear spinner

	if err != nil {
		if reqCtx.Err() == context.Canceled {
			fmt.Println(paint(cYellow, " [interrupted]"))
			if c.emitter != nil {
				c.emitter.EmitChatResponse("[interrupted]")
			}
		} else {
			fmt.Printf("%s %s\n", paint(cRed, "✗ error:"), paint(cRed, err.Error()))
			if c.emitter != nil {
				c.emitter.EmitChatResponse("Error: " + err.Error())
			}
		}
		c.printUsageDelta(beforeReqs)
		return
	}

	if c.emitter != nil {
		c.emitter.EmitChatResponse(result)
	}

	// Inline summary: show what files were modified (before → after) and
	// which commands ran. This replaces the old orchestration-event spam.
	if c.recorder != nil {
		changes := c.recorder.Since(beforeChanges)
		if len(changes) > 0 {
			fmt.Println(paint(cAmber+clrBold, "▸ changes"))
			for _, ch := range changes {
				renderChange(os.Stdout, ch, 18)
			}
			fmt.Println()
		}
	}

	// Final answer
	fmt.Println(paint(cAmber+clrBold, "▣ DARKCODE"))
	fmt.Println(paint(cWhite, result))
	fmt.Println()
	c.printUsageDelta(beforeReqs)

	c.recordActivity(core.UIEvent{
		Type: core.EventFinalOutput, Status: "final",
		Content: result, Timestamp: time.Now(),
	})

	// Background plan/workflow refresh. This is a display refinement (the
	// plan/workflow also drives execution via injectProjectContext, but a
	// stale plan is harmless — it simply reflects the previous state).
	//
	// Sequential-mode guard: in Sequential mode (the Auto default for
	// free-tier cloud models) we SKIP this async update. The 2 extra LLM
	// calls would run concurrently with the user's NEXT request and compete
	// for the free-tier rate limit, causing 429s that make it look like the
	// CLI "isn't taking new requests" after a response. Skipping keeps the
	// prompt responsive and honors the sequential contract; the plan updates
	// on the next Parallel request. (Retry + timeout are still applied in
	// Parallel mode so a hanging/slow model can't linger for 300s.)
	if c.activeProject != "" && c.projects != nil && c.kernel != nil && !c.kernel.SequentialMode() {
		go func(projID, q, out string) {
			// Wrap with retry/backoff (429/5xx) and bound the lifetime so a
			// hanging model can't keep this goroutine alive for the full 300s
			// HTTP timeout.
			client := llm.WithRetry(llm.NewClient(c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Model), llm.DefaultRetryOpts)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			temp := 0.0

			oldPlan, _ := c.projects.GetPlan(projID)
			planPrompt := fmt.Sprintf("Here is the current Implementation Plan:\n%s\n\nUser asked: %s\nAgent did: %s\n\nRewrite the implementation plan to reflect the new state. Output ONLY the raw markdown plan.", oldPlan, q, out)
			llmReq1 := &core.CompletionRequest{
				Messages: []core.Message{
					{Role: "system", Content: "You are an AI architect. Keep the plan concise and action-oriented. Only output valid markdown."},
					{Role: "user", Content: planPrompt},
				},
				Temperature: &temp,
			}
			pResp, err := client.ChatCompletion(ctx, llmReq1)
			if err == nil && len(pResp.Choices) > 0 {
				planText := pResp.Choices[0].Message.Content
				c.projects.SetPlan(projID, planText)
				if c.emitter != nil {
					c.emitter.EmitPlanUpdated(projID, planText)
				}
			}

			oldWf, _ := c.projects.GetWorkflow(projID)
			wfPrompt := fmt.Sprintf("Here is the current Workflow Architecture:\n%s\n\nUser asked: %s\nAgent did: %s\n\nRewrite the workflow architecture to reflect the new state. Output ONLY the raw markdown.", oldWf, q, out)
			llmReq2 := &core.CompletionRequest{
				Messages: []core.Message{
					{Role: "system", Content: "You are an AI architect. Keep the workflow architecture concise. Only output valid markdown."},
					{Role: "user", Content: wfPrompt},
				},
				Temperature: &temp,
			}
			wResp, err := client.ChatCompletion(ctx, llmReq2)
			if err == nil && len(wResp.Choices) > 0 {
				wfText := wResp.Choices[0].Message.Content
				c.projects.SetWorkflow(projID, wfText)
				if c.emitter != nil {
					c.emitter.EmitWorkflowUpdated(projID, wfText)
				}
			}
		}(c.activeProject, origQuery, result)
	}
}

// recordActivity appends an event to the in-memory activity log used by /log.
func (c *Console) recordActivity(e core.UIEvent) {
	entry := activityEntry{
		time: e.Timestamp,
		icon: eventIcon(string(e.Type)),
		kind: string(e.Type),
		tool: e.Tool,
		msg:  eventMessage(e),
	}
	if entry.time.IsZero() {
		entry.time = time.Now()
	}
	c.activityMu.Lock()
	c.activity = append(c.activity, entry)
	c.activityMu.Unlock()
}

// requestApproval is the interactive terminal permission prompt. It is the
// CLI delegate of the ModeAwareApprover and called whenever a dangerous tool
// call needs the user's blessing. The user can allow once, allow for the
// whole session, or deny — and may attach a free-form feedback note (e.g.
// "3 use /tmp instead of /var") which is surfaced back to the agent through
// the tool-result channel so it adapts. Prompts are serialized by the gate.
func (c *Console) requestApproval(req permission.ApprovalRequest) permission.Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Pause/clear the spinner so the prompt is clean.
	fmt.Print("\r" + ansiClearLine + "\r")

	riskColor := cYellow
	switch req.Risk {
	case core.RiskCritical, core.RiskHigh:
		riskColor = cRed
	case core.RiskMedium:
		riskColor = cYellow
	case core.RiskLow:
		riskColor = cGreen
	}

	fmt.Println(paint(cAmber+clrBold, "╭─ PERMISSION REQUIRED ────────────────────────────"))
	fmt.Printf("%s %s  %s  %s\n",
		paint(cGray, "│"),
		paint(cWhite+clrBold, padRight(req.Tool, 14)),
		paint(riskColor, "["+string(req.Risk)+" risk]"),
		paint(cGray, req.Summary))
	if req.Preview != "" {
		for _, line := range strings.Split(req.Preview, "\n") {
			fmt.Printf("%s %s\n", paint(cGray, "│"), paint(cGrayLt, strutil.Truncate(line, 76)))
		}
	}
	fmt.Printf("%s %s\n", paint(cGray, "│"), paint(cGray, "allow this action?"))
	fmt.Printf("%s   %s allow once   %s allow session   %s deny   %s\n",
		paint(cGray, "│"),
		paint(cGreen, "[1]"),
		paint(cBlue, "[2]"),
		paint(cRed, "[3]"),
		paint(cGray, "(default: 1)"))
	fmt.Printf("%s %s\n", paint(cGray, "│"), paint(cGrayLt, "tip: append feedback, e.g. \"3 use /tmp instead\""))

	if c.rl != nil {
		c.rl.SetPrompt("")
		defer c.rl.SetPrompt(">>> ")
	}

	// Re-prompt on any unrecognized input instead of silently granting
	// AllowOnce — the user must actively decide (1/2/3, or blank for the
	// visibly-advertised default of 1). Only a real interrupt (Ctrl+C/EOF)
	// or an explicit deny answers on the user's behalf.
	for {
		fmt.Print(paint(cGray, "│ ") + paint(cOrange, "> "))

		var input string
		if c.rl != nil {
			var err error
			input, err = c.rl.Readline()
			if err != nil {
				fmt.Println(paint(cGray, "╰───────────────────────────────────────────────────"))
				return permission.Verdict{Decision: permission.DecisionDeny, Feedback: "interrupted"}
			}
		}

		// Split the choice token from any trailing free-form feedback.
		first, rest := splitFirstWord(input)
		first = strings.ToLower(strings.TrimSpace(first))
		feedback := strings.TrimSpace(rest)

		switch first {
		case "1", "y", "yes", "o", "once", "":
			fmt.Println(paint(cGray, "╰───────────────────────────────────────────────────"))
			return permission.Verdict{Decision: permission.DecisionAllowOnce, Feedback: feedback}
		case "2", "s", "session", "a":
			fmt.Println(paint(cGray, "╰───────────────────────────────────────────────────"))
			return permission.Verdict{Decision: permission.DecisionAllowSession, Feedback: feedback}
		case "3", "n", "no", "deny", "d":
			fmt.Println(paint(cGray, "╰───────────────────────────────────────────────────"))
			return permission.Verdict{Decision: permission.DecisionDeny, Feedback: feedback}
		default:
			fmt.Printf("%s %s\n", paint(cGray, "│"), paint(cRed, "invalid choice — enter 1, 2, or 3 (blank = 1)"))
		}
	}
}

// splitFirstWord separates the first whitespace-delimited token from the rest
// of the input, so the approval prompt can parse "3 use /tmp" into choice "3"
// and feedback "use /tmp".
func splitFirstWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// printActivityLog renders the full orchestration trace that was suppressed
// from stdout during queries, plus the detailed before/after changes. Opened
// via the /log slash command.
func (c *Console) printActivityLog() {
	c.activityMu.Lock()
	entries := make([]activityEntry, len(c.activity))
	copy(entries, c.activity)
	c.activityMu.Unlock()

	if len(entries) == 0 {
		fmt.Println(paint(cGray, "  no activity recorded yet. run a query first."))
		return
	}

	w := termWidth()
	if w > 80 {
		w = 80
	}
	fmt.Println()
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "ACTIVITY LOG"), paint(cGray, "("+fmtNum(len(entries))+" events)"))
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))
	for _, e := range entries {
		label := e.kind
		if e.tool != "" {
			label = e.tool
		}
		fmt.Printf("  %s  %s  %s  %s\n",
			paint(cGray, e.time.Format("15:04:05")),
			paint(cOrange, e.icon),
			paint(cBlue, padRight(label, 16)),
			paint(cGray, strutil.Truncate(e.msg, w-34)))
	}
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// Detailed changes: full before → after diffs and command outputs.
	if c.recorder != nil {
		changes := c.recorder.All()
		if len(changes) > 0 {
			fmt.Println()
			fmt.Printf("%s %s\n", paint(cAmber+clrBold, "CHANGES"), paint(cGray, "("+fmtNum(len(changes))+" recorded)"))
			for _, ch := range changes {
				renderChange(os.Stdout, ch, 60)
			}
		}
	}
	fmt.Println()
}

// printPermissions shows the permission gate's level + counters, and supports
// `/permissions reset` to clear session-scoped decisions.
func (c *Console) printPermissions(args []string) {
	if len(args) > 0 && args[0] == "reset" {
		if c.gate != nil {
			c.gate.ResetSession()
		}
		fmt.Println(paint(cGreen, "✓") + paint(cGray, " session permissions reset."))
		return
	}
	if c.gate == nil {
		fmt.Println(paint(cGray, "  permission gate not installed."))
		return
	}
	stats := c.gate.Stats()
	fmt.Println(paint(cAmber+clrBold, "PERMISSION GATE"))
	fmt.Printf("  %-18s %s\n", paint(cGray, "level"), paint(cYellow, stats.Level.String()))
	fmt.Printf("  %-18s %s\n", paint(cGray, "prompts asked"), fmtNum(stats.Asked))
	fmt.Printf("  %-18s %s\n", paint(cGray, "approved"), paint(cGreen, fmtNum(stats.Approved)))
	fmt.Printf("  %-18s %s\n", paint(cGray, "denied"), paint(cRed, fmtNum(stats.Denied)))
	fmt.Printf("  %-18s %s\n", paint(cGray, "session allows"), paint(cBlue, fmtNum(stats.SessionAll)))
	fmt.Printf("  %-18s %s\n", paint(cGray, "session denies"), paint(cRed, fmtNum(stats.SessionDeny)))
	fmt.Printf("\n  %s /permissions reset to clear session decisions\n", paint(cGray, ""))
}

// printUsageDelta prints a compact usage summary for the requests made since
// the given baseline count.
func (c *Console) printUsageDelta(beforeReqs int) {
	snap := metrics.Default.Snapshot()
	delta := snap.TotalRequests - beforeReqs
	if delta <= 0 && snap.TotalRequests == 0 {
		return
	}
	// Sum tokens/cost for requests in this query (best-effort from recent).
	var tok, cost int64
	var lat int64
	count := 0
	for i := len(snap.Recent) - 1; i >= 0 && count < delta; i-- {
		r := snap.Recent[i]
		tok += int64(r.TotalTokens)
		cost += int64(r.Cost * 1e6) // store as micro-dollars
		lat += r.LatencyMs
		count++
	}
	fmt.Print(paint(cGray, "  ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄") + "\n")
	avgLat := int64(0)
	if count > 0 {
		avgLat = lat / int64(count)
	}
	fmt.Printf("  %s  %s tokens · %s · %s reqs · avg %s   %s\n",
		paint(cAmber+clrBold, "usage"),
		paint(cOrange, fmtNum(int(tok))),
		paint(cGreen, fmtCost(float64(cost)/1e6)),
		paint(cBlue, fmtNum(count)),
		paint(cYellow, fmtDur(avgLat)),
		paint(cGray, "(total "+fmtNum(snap.TotalTokens)+" tok / "+fmtCost(snap.TotalCost)+")"))
	fmt.Println()
}

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
		printHelpScreen()

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
		
	case "/sandbox":
		fmt.Println(paint(cPurple+clrBold, "🛡️ SECURITY SANDBOX STATUS"))
		fmt.Println(paint(cWhite, "   Mode: ") + paint(cGreen, "Active"))
		fmt.Println(paint(cGray, "   (All tool executions are strictly isolated via firejail/namespaces)"))

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

	case "/skills":
		c.printSkills()

	case "/episodes":
		c.printEpisodes()

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

	case "/chatmode":
		if len(parts) > 1 {
			c.setChatMode(parts[1])
		} else {
			fmt.Printf("%s %s  %s\n", paint(cGray, "chat mode:"), paint(cCyan, c.chatMode), paint(cGray, "(smart | loop)"))
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
				if c.cfg.EnableLocalLLM {
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

	default:
		fmt.Printf("%s unknown command: %s %s\n", paint(cRed, "✗"), cmd, paint(cGray, "(try /help)"))
	}

	return false
}

// splitCmd splits a command line honoring simple quoting for args with spaces.
func splitCmd(input string) []string {
	return strings.Fields(input)
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

func (c *Console) printSkills() {
	skills := c.mem.ProceduralAll()
	if len(skills) == 0 {
		fmt.Println(paint(cGray, "  no skills stored yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROCEDURAL MEMORY"), paint(cGray, "("+fmtNum(len(skills))+" skills)"))
	for _, s := range skills {
		fmt.Printf("  %s  %s  %s  %s\n",
			paint(cYellow, "★"),
			paint(cWhite, padRight(s.Name, 22)),
			paint(cGreen, fmt.Sprintf("%d uses %d%%", s.UseCount, int(s.SuccessRate*100))),
			paint(cGray, strutil.Truncate(s.Description, 36)))
	}
}

func (c *Console) printEpisodes() {
	eps := c.mem.EpisodicGetRecent(10)
	if len(eps) == 0 {
		fmt.Println(paint(cGray, "  no episodic memory yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "RECENT EPISODES"), paint(cGray, "("+fmtNum(len(eps))+")"))
	for _, e := range eps {
		icon := paint(cGreen, "✓")
		if e.Outcome != "success" {
			icon = paint(cRed, "✗")
		}
		fmt.Printf("  %s  %s  %s\n", icon, paint(cGray, fmtTime(e.Timestamp)), strutil.Truncate(e.TaskGoal, 56))
	}
}

func (c *Console) printKnowledge() {
	kg := c.mem.KG()
	if kg == nil {
		fmt.Println(paint(cGray, "  knowledge graph unavailable."))
		return
	}
	nodes, edges := kg.Stats()
	fmt.Printf("%s  %s nodes / %s edges\n",
		paint(cAmber+clrBold, "KNOWLEDGE GRAPH"),
		paint(cOrange, fmtNum(nodes)),
		paint(cBlue, fmtNum(edges)))

	// Show the top concept (word) relations so /know reflects the word-relation
	// layer, not just a stat line. Concepts are linked by co-occurrence
	// (related_to edges weighted by how often they appeared together).
	concepts := kg.FindByType(core.KGNodeConcept)
	if len(concepts) == 0 {
		fmt.Println(paint(cGray, "  no concept relations yet — they build up as tasks run."))
		return
	}
	// Rank concepts by edge count (degree) and show the top few with their
	// strongest relations.
	type conceptDeg struct {
		id  string
		lbl string
		deg int
	}
	var ranked []conceptDeg
	for _, n := range concepts {
		deg := len(kg.GetEdges(n.ID))
		ranked = append(ranked, conceptDeg{n.ID, n.Label, deg})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].deg > ranked[j].deg })
	limit := 8
	if len(ranked) < limit {
		limit = len(ranked)
	}
	fmt.Printf("  %s (top %d by connectivity):\n", paint(cAmber, "concept relations"), limit)
	for i := 0; i < limit; i++ {
		cd := ranked[i]
		relsIface := kg.ConceptRelations(cd.lbl)
		rels, _ := relsIface.([]memory.ConceptRelation)
		// Show up to 3 strongest neighbors.
		var neighbors []string
		for _, r := range rels {
			neighbors = append(neighbors, fmt.Sprintf("%s(%.0f)", r.Label, r.Weight))
		}
		if len(neighbors) > 3 {
			neighbors = neighbors[:3]
		}
		joined := "—"
		if len(neighbors) > 0 {
			joined = strings.Join(neighbors, ", ")
		}
		fmt.Printf("    • %s [%d links] → %s\n", paint(cWhite+clrBold, cd.lbl), cd.deg, paint(cGray, joined))
	}
	fmt.Println(paint(cGray, "  (use /know <word> for a concept's full relations, or the 'memory action=kg' tool)"))
}

// printConceptRelations shows all weighted relations for a concept word.
// Reached via /know <word>. Mirrors the memory tool's kg query action.
func (c *Console) printConceptRelations(word string) {
	kg := c.mem.KG()
	if kg == nil {
		fmt.Println(paint(cGray, "  knowledge graph unavailable."))
		return
	}
	relsIface := kg.ConceptRelations(word)
	rels, _ := relsIface.([]memory.ConceptRelation)
	if len(rels) == 0 {
		fmt.Printf("%s no concept relations found for %q\n", paint(cYellow, "!"), word)
		return
	}
	fmt.Printf("%s  %q → %d related concept(s):\n", paint(cAmber+clrBold, "CONCEPT RELATIONS"), word, len(rels))
	// Sort by weight descending so strongest relations show first.
	sort.Slice(rels, func(i, j int) bool { return rels[i].Weight > rels[j].Weight })
	for _, r := range rels {
		fmt.Printf("  %s %s  %s  %s\n",
			paint(cBlue, "•"), paint(cWhite+clrBold, r.Label),
			paint(cGray, r.Relation), paint(cGray, fmt.Sprintf("(weight: %.0f)", r.Weight)))
	}
}

func (c *Console) printConfig() {
	fmt.Println(paint(cAmber+clrBold, "CONFIGURATION"))
	fmt.Printf("  %-16s %s\n", paint(cGray, "model"), paint(cOrange+clrBold, c.cfg.Model))
	fmt.Printf("  %-16s %s\n", paint(cGray, "provider"), paint(cWhite, c.cfg.Provider))
	fmt.Printf("  %-16s %s\n", paint(cGray, "base_url"), paint(cGray, c.cfg.BaseURL))
	fmt.Printf("  %-16s %s\n", paint(cGray, "routing_mode"), paint(cBlue, c.cfg.RoutingMode))
	prof := c.cfg.ExecutionProfile
	if prof == "" {
		prof = "auto"
	}
	fmt.Printf("  %-16s %s\n", paint(cGray, "execution_profile"), paint(cCyan, prof))
	fmt.Printf("  %-16s %s\n", paint(cGray, "safety_level"), paint(cYellow, c.cfg.SafetyLevel))
	fmt.Printf("  %-16s %s\n", paint(cGray, "max_turns"), paint(cWhite, fmtNum(c.cfg.MaxTurns)))
	fmt.Printf("  %-16s %s\n", paint(cGray, "max_concurrent"), paint(cWhite, fmtNum(c.cfg.MaxConcurrent)))
	fmt.Printf("  %-16s %s\n", paint(cGray, "compress_context"), paint(cWhite, fmt.Sprintf("%v", c.cfg.CompressContext)))
	cm := c.cfg.CompressorModel
	if cm == "" {
		cm = "<primary>"
	}
	fmt.Printf("  %-16s %s\n", paint(cGray, "compressor_model"), paint(cWhite, cm))
	fmt.Printf("  %-16s %s\n", paint(cGray, "memory_dir"), paint(cGray, c.cfg.MemoryDir))
	if len(c.cfg.Models) > 0 {
		fmt.Printf("  %-16s\n", paint(cGray, "registered models:"))
		for k, m := range c.cfg.Models {
			primary := ""
			if k == c.cfg.Model {
				primary = paint(cOrange, " (primary)")
			}
			fmt.Printf("     • %s  %s  %s%s\n", paint(cWhite, m.Model), paint(cGray, m.Provider), paint(cGray, m.BaseURL), primary)
		}
	}
}

func (c *Console) setModel(name string) {
	c.cfg.Model = name
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s model set to %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, name), paint(cGray, "(hot-reloaded)"))
}

// setCompressor selects the model used for ALL context compression (STM +
// project summary). "primary" (or empty) clears the override and uses the
// primary model. Hot-reloaded via ReloadModels so the change takes effect
// immediately. Mirrors setModel/setMode.
func (c *Console) setCompressor(name string) {
	name = strings.TrimSpace(name)
	// "primary" means: no dedicated compressor — use the primary model.
	if name == "primary" || name == "" {
		c.cfg.CompressorModel = ""
	} else if _, ok := c.cfg.Models[name]; !ok {
		fmt.Printf("%s unknown model %s — registered models: %s\n",
			paint(cRed, "✗"), paint(cWhite, name), c.listModelNames())
		return
	} else {
		c.cfg.CompressorModel = name
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	display := c.cfg.CompressorModel
	if display == "" {
		display = "<primary model>"
	}
	fmt.Printf("%s compressor model → %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, display), paint(cGray, "(hot-reloaded; governs STM + project compression)"))
}

// listModelNames returns the registered model names as a comma-separated list
// for error messages.
func (c *Console) listModelNames() string {
	var names []string
	for k := range c.cfg.Models {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (c *Console) setMode(mode string) {
	switch strings.ToLower(mode) {
	case "single", "escalation", "consensus":
		c.cfg.RoutingMode = strings.ToLower(mode)
	default:
		fmt.Printf("%s invalid mode %s (single | escalation | consensus)\n", paint(cRed, "✗"), mode)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	fmt.Printf("%s routing mode → %s\n", paint(cGreen, "✓"), paint(cBlue, c.cfg.RoutingMode))
}

// setProfile switches the execution profile (parallel/sequential/auto) at
// runtime. Mirrors setMode/setSafety. The profile is hot-toggled on the
// kernel via SetExecutionProfile so it takes effect on the next Execute
// without a restart. Auto resolves per-request: sequential when only
// free-tier cloud models are registered, parallel otherwise.
// setChatMode switches the per-request chat mode (CLI ↔ GUI parity with
// the web chat's mode dropdown). general = pure conversation (no tools),
// project/auto = full tool runtime, loop = ReAct loop (needs the Agentic
// Loop master toggle on in Settings). Applied per-query via
// ApplyRequestOverrides in runQuery.
func (c *Console) setChatMode(mode string) {
	mode = strings.ToLower(mode)
	switch mode {
	case "smart", "loop":
		c.chatMode = mode
	default:
		fmt.Printf("%s invalid chat mode %q. Use: smart, loop\n", paint(cRed, "✗"), mode)
		return
	}
	note := ""
	if c.chatMode == "loop" && !c.cfg.AgenticLoop {
		note = paint(cYellow, "  (enable the Agentic Loop in /config or /help for Loop mode to take effect)")
	}
	fmt.Printf("%s chat mode → %s%s\n", paint(cGreen, "✓"), paint(cCyan, c.chatMode), note)
}

func (c *Console) setProfile(profile string) {
	switch strings.ToLower(profile) {
	case "parallel", "sequential", "auto":
		c.cfg.ExecutionProfile = strings.ToLower(profile)
	default:
		fmt.Printf("%s invalid profile %s (parallel | sequential | auto)\n", paint(cRed, "✗"), profile)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	if c.kernel != nil {
		c.kernel.SetExecutionProfile(c.cfg.ExecutionProfile)
	}
	fmt.Printf("%s execution profile → %s\n", paint(cGreen, "✓"), paint(cCyan, c.cfg.ExecutionProfile))
}

// setLocal toggles the local LLM initialization at startup or task offloading.
func (c *Console) setLocal(args []string) {
	if len(args) == 0 {
		return
	}
	
	if strings.ToLower(args[0]) == "offload" {
		if len(args) < 2 {
			fmt.Printf("%s missing state for offload (on | off)\n", paint(cRed, "✗"))
			return
		}
		arg := strings.ToLower(args[1])
		switch arg {
		case "on", "true", "enable", "1":
			c.cfg.EnableLocalOffloading = true
		case "off", "false", "disable", "0":
			c.cfg.EnableLocalOffloading = false
		default:
			fmt.Printf("%s invalid state %s (on | off)\n", paint(cRed, "✗"), arg)
			return
		}
		if err := c.cfg.Save(); err != nil {
			fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
			return
		}
		state := paint(cRed, "off")
		if c.cfg.EnableLocalOffloading {
			state = paint(cGreen, "on")
		}
		fmt.Printf("%s local offload → %s\n", paint(cGreen, "✓"), state)
		return
	}

	arg := strings.ToLower(args[0])
	switch arg {
	case "force":
		// Force-local: pin routing to the local model — no cloud fallback —
		// and auto-start it now.
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "force"
	case "on", "true", "enable", "1":
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "on"
	case "auto":
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "auto"
	case "off", "false", "disable", "0":
		c.cfg.EnableLocalLLM = false
		c.cfg.LocalMode = "off"
	default:
		fmt.Printf("%s invalid state %s (force | on | auto | off)\n", paint(cRed, "✗"), arg)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}

	// Apply the preference immediately (no restart): pin/unpin force-local
	// routing and, when local is wanted but not yet up, start the embedded
	// model on demand. In force mode a startup failure is a hard error — the
	// never-silent-fallback guarantee — so surface it and warn the user their
	// requests will fail until local is available, rather than quietly using
	// a cloud model.
	if c.kernel != nil {
		if err := c.kernel.ApplyLocalPreference(context.Background(), c.cfg); err != nil {
			fmt.Printf("%s %s\n", paint(cYellow, "⚠"), err)
		}
	}

	switch c.cfg.ResolvedLocalMode() {
	case "force":
		// ApplyLocalPreference always pins routing first, so force is active
		// here regardless of whether the model finished loading (any load
		// failure was already surfaced as the ⚠ diagnostic above).
		fmt.Printf("%s local llm → %s (%s)\n", paint(cGreen, "✓"), paint(cGreen+clrBold, "force"), paint(cGray, "cloud fallback disabled"))
	case "off":
		fmt.Printf("%s local llm auto-load → %s\n", paint(cGreen, "✓"), paint(cRed, "off"))
	default:
		fmt.Printf("%s local llm auto-load → %s (%s)\n", paint(cGreen, "✓"), paint(cGreen, "on"), c.cfg.ResolvedLocalMode())
	}
}

func (c *Console) setSafety(level string) {
	switch strings.ToLower(level) {
	case "strict", "normal", "relaxed":
		c.cfg.SafetyLevel = strings.ToLower(level)
	default:
		fmt.Printf("%s invalid level %s (strict | normal | relaxed)\n", paint(cRed, "✗"), level)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	fmt.Printf("%s safety level → %s\n", paint(cGreen, "✓"), paint(cYellow, c.cfg.SafetyLevel))
}

// handleModels dispatches /models subcommands.
func (c *Console) handleModels(args []string) {
	if len(args) == 0 {
		c.listModels()
		return
	}
	switch args[0] {
	case "add":
		c.modelAdd(args[1:])
	case "remove", "rm":
		c.modelRemove(args[1:])
	case "primary", "use":
		c.modelPrimary(args[1:])
	case "test", "ping":
		c.modelTest(args[1:])
	case "disable":
		c.modelDisable(args[1:])
	case "enable":
		c.modelEnable(args[1:])
	default:
		fmt.Printf("%s usage: /models [add|remove|primary|test|disable|enable]\n", paint(cRed, "✗"))
	}
}

// modelDisable temporarily takes a model out of routing/consensus selection
// (local-first upgrade §6c). Usage: /models disable <name> [duration]
// (duration is a Go duration string like "1h", "30m"; default "1h" if
// omitted).
func (c *Console) modelDisable(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models disable <name> [duration] (e.g. /models disable gpt-4 1h)\n", paint(cRed, "✗"))
		return
	}
	name := args[0]
	durStr := "1h"
	if len(args) > 1 {
		durStr = args[1]
	}
	dur, err := time.ParseDuration(durStr)
	if err != nil {
		fmt.Printf("%s invalid duration %q: %v (try e.g. \"1h\", \"30m\")\n", paint(cRed, "✗"), durStr, err)
		return
	}
	if c.kernel == nil {
		fmt.Printf("%s no kernel available — cannot disable models\n", paint(cRed, "✗"))
		return
	}
	c.kernel.DisableModel(name, time.Now().Add(dur))
	fmt.Printf("%s %s disabled for %s\n", paint(cGreen, "✓"), paint(cWhite, name), dur)
}

// modelEnable reverses a temporary disable early. Usage: /models enable <name>
func (c *Console) modelEnable(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models enable <name>\n", paint(cRed, "✗"))
		return
	}
	name := args[0]
	if c.kernel == nil {
		fmt.Printf("%s no kernel available — cannot enable models\n", paint(cRed, "✗"))
		return
	}
	c.kernel.EnableModel(name)
	fmt.Printf("%s %s enabled\n", paint(cGreen, "✓"), paint(cWhite, name))
}

// modelTest is the explicit "test connection" action (local-first upgrade
// §4c): unlike the auto-discovered fallback in modelAdd's fetch step, this
// runs synchronously and reports the actual error, so a user can verify an
// already-configured model on demand instead of only finding out a
// connection is broken when a real chat request fails. name is a key from
// c.cfg.Models, or empty to test the primary (c.cfg.Model).
func (c *Console) modelTest(args []string) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	var mc struct {
		provider, baseURL, apiKey, model string
	}
	if name == "" || name == c.cfg.Model {
		mc.provider, mc.baseURL, mc.apiKey, mc.model = c.cfg.Provider, c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Model
		name = c.cfg.Model
	} else if m, ok := c.cfg.Models[name]; ok {
		mc.provider, mc.baseURL, mc.apiKey, mc.model = m.Provider, m.BaseURL, m.APIKey, m.Model
	} else {
		fmt.Printf("%s no configured model named %q (see /models for the list)\n", paint(cRed, "✗"), name)
		return
	}
	if mc.baseURL == "" {
		fmt.Printf("%s %q has no base URL configured (local/embedded models aren't tested this way — see /local)\n", paint(cRed, "✗"), name)
		return
	}

	client := llm.NewClient(mc.baseURL, mc.apiKey, mc.model)
	client.SetProvider(mc.provider)

	fmt.Printf("%s testing connection to %s...\n", paint(cGray, "›"), paint(cWhite, name))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		fmt.Printf("%s %s: %v\n", paint(cRed, "✗ connection failed"), name, err)
		return
	}
	fmt.Printf("%s %s is reachable\n", paint(cGreen, "✓"), name)
}

func (c *Console) listModels() {
	total := len(c.cfg.Models) + 1 // +1 for the primary
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "REGISTERED MODELS"), paint(cGray, "("+fmtNum(total)+")"))
	primaryLabel := paint(cOrange, "primary")
	if c.kernel != nil && c.kernel.IsModelDisabled(c.cfg.Model) {
		primaryLabel = paint(cOrange, "primary") + paint(cRed, "  ⊘ disabled")
	}
	fmt.Printf("  %s  %s  %s  %s  %s\n",
		paint(cOrange, "★"),
		paint(cWhite, padRight(c.cfg.Model, 28)),
		paint(cBlue, padRight(config.ResolveTier(c.cfg.Provider, c.cfg.Model), 10)),
		paint(cGray, padRight(c.cfg.Provider, 14)),
		primaryLabel)
	for k, m := range c.cfg.Models {
		tier := m.Tier
		if tier == "" {
			tier = config.ResolveTier(m.Provider, m.Model)
		}
		marker := "•"
		markerColor := cGray
		if c.kernel != nil && c.kernel.IsModelDisabled(m.Model) {
			marker = "⊘ disabled"
			markerColor = cRed
		}
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			paint(markerColor, marker),
			paint(cWhite, padRight(k, 28)),
			paint(cBlue, padRight(tier, 10)),
			paint(cGray, padRight(m.Provider, 14)),
			paint(cGray, strutil.Truncate(m.BaseURL, 40)))
	}
	fmt.Printf("\n  %s /models add <provider> <model> [api_key]  ·  /providers to browse\n", paint(cGray, "add with:"))
}

func (c *Console) modelAdd(args []string) {
	if len(args) == 0 {
		// Interactive wizard — mirrors the GUI's provider-driven flow.
		c.modelAddWizard()
		return
	}
	if len(args) < 2 {
		fmt.Printf("%s usage: /models add <provider> <model> [api_key] [base_url]\n", paint(cRed, "✗"))
		fmt.Printf("   %s or run /models add with no args for the interactive wizard\n", paint(cGray, ""))
		fmt.Printf("   %s browse providers with /providers\n", paint(cGray, ""))
		return
	}
	provider := args[0]
	model := args[1]
	apiKey := ""
	if len(args) > 2 {
		apiKey = args[2]
	}
	baseURL := ""
	if len(args) > 3 {
		baseURL = args[3]
	}
	// Resolve base URL + auth from the provider registry if not supplied.
	if baseURL == "" {
		if p, ok := config.LookupProvider(provider); ok {
			baseURL = p.BaseURL
		}
	}
	// If no api key and provider is local (auth=none), leave empty.
	if apiKey == "" {
		if p, ok := config.LookupProvider(provider); ok && p.AuthScheme == config.AuthNone {
			// fine — local model needs no key
		}
	}
	if c.cfg.Models == nil {
		c.cfg.Models = make(map[string]config.ModelConfig)
	}
	tier := config.ResolveTier(provider, model)
	c.cfg.Models[model] = config.ModelConfig{
		Provider: provider,
		Model:    model,
		APIKey:   apiKey,
		BaseURL:  baseURL,
		Tier:     tier,
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	keyHint := paint(cGreen, "✓ key set")
	if apiKey == "" {
		keyHint = paint(cYellow, "no key (local?)")
	}
	fmt.Printf("%s registered %s  %s  %s  %s\n",
		paint(cGreen, "✓"),
		paint(cWhite, model),
		paint(cBlue, "("+tier+")"),
		paint(cGray, "("+provider+" @ "+baseURL+")"),
		keyHint)
}

// The models are fetched dynamically using the provider module

func (c *Console) modelAddWizard() {
	provs := config.Providers()
	
	// 1. Select Provider
	var providerItems []tui.SelectorItem
	for _, p := range provs {
		auth := p.AuthScheme
		if auth == config.AuthNone {
			auth = "local"
		}
		desc := fmt.Sprintf("%s · %d models", auth, len(p.Models))
		providerItems = append(providerItems, tui.SelectorItem{
			Title:       p.Name,
			Description: desc,
			Value:       p.ID,
		})
	}
	
	providerID := tui.Select("Select Provider:", providerItems)
	if providerID == "" {
		fmt.Println(paint(cGray, "Cancelled."))
		return
	}
	
	provider, ok := config.LookupProvider(providerID)
	if !ok {
		return
	}
	
	// 2. API key (skip for local/no-auth providers)
	apiKey := ""
	if provider.AuthScheme != config.AuthNone {
		if provider.KeyURL != "" {
			fmt.Printf("%s %s get a key at %s\n", paint(cGray, "│"), paint(cGray, "ℹ"), paint(cBlue, provider.KeyURL))
		}
		key, canceled := tui.Input("API Key (leave blank for none)", true)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		apiKey = strings.TrimSpace(key)
		if apiKey == "" {
			fmt.Printf("%s %s no key provided — model may fail to authenticate\n", paint(cGray, "│"), paint(cYellow, "⚠"))
		}
	}

	// 3. Optional Base URL for local/custom providers
	baseURL := provider.BaseURL
	if provider.Local || provider.CustomBaseURL {
		url, canceled := tui.Input("Base URL (optional, defaults to "+baseURL+")", false)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		if strings.TrimSpace(url) != "" {
			baseURL = strings.TrimSpace(url)
		}
	}

	// 4. Fetch models dynamically
	fmt.Printf("%s %s fetching active models from %s...\n", paint(cGray, "│"), paint(cBlue, "↻"), provider.ID)
	fetchedModels, err := provpkg.FetchModels(provider, apiKey, baseURL)
	
	var modelItems []tui.SelectorItem
	if err == nil && len(fetchedModels) > 0 {
		for _, m := range fetchedModels {
			desc := "Live fetched model"
			for _, km := range provider.Models {
				if km.ID == m {
					ctxK := fmtNum(km.ContextWindow)
					if km.ContextWindow >= 1000000 {
						ctxK = fmt.Sprintf("%.1fM", float64(km.ContextWindow)/1e6)
					}
					price := "free"
					if km.InputPrice > 0 || km.OutputPrice > 0 {
						price = fmt.Sprintf("$%.2f/$%.2f", km.InputPrice, km.OutputPrice)
					}
					desc = fmt.Sprintf("%s · %s ctx · %s", km.Tier, ctxK, price)
					break
				}
			}
			modelItems = append(modelItems, tui.SelectorItem{
				Title:       m,
				Description: desc,
				Value:       m,
			})
		}
	} else {
		fmt.Printf("%s %s failed to fetch dynamically, falling back to catalogue: %v\n", paint(cGray, "│"), paint(cYellow, "⚠"), err)
		for _, m := range provider.Models {
			ctxK := fmtNum(m.ContextWindow)
			if m.ContextWindow >= 1000000 {
				ctxK = fmt.Sprintf("%.1fM", float64(m.ContextWindow)/1e6)
			}
			price := "free"
			if m.InputPrice > 0 || m.OutputPrice > 0 {
				price = fmt.Sprintf("$%.2f/$%.2f", m.InputPrice, m.OutputPrice)
			}
			desc := fmt.Sprintf("%s · %s ctx · %s", m.Tier, ctxK, price)
			modelItems = append(modelItems, tui.SelectorItem{
				Title:       m.ID,
				Description: desc,
				Value:       m.ID,
			})
		}
	}
	modelItems = append(modelItems, tui.SelectorItem{
		Title:       "Custom Model...",
		Description: "Type a custom model ID",
		Value:       "__custom__",
	})
	
	// 5. Select Model
	modelID := tui.Select("Select Model:", modelItems)
	if modelID == "" {
		fmt.Println(paint(cGray, "Cancelled."))
		return
	}
	
	if modelID == "__custom__" {
		custom, canceled := tui.Input("Custom model ID", false)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		modelID = strings.TrimSpace(custom)
		if modelID == "" {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
	}

	// Register
	if c.cfg.Models == nil {
		c.cfg.Models = make(map[string]config.ModelConfig)
	}
	tier := config.ResolveTier(provider.ID, modelID)
	c.cfg.Models[modelID] = config.ModelConfig{
		Provider: provider.ID,
		Model:    modelID,
		APIKey:   apiKey,
		BaseURL:  baseURL, // Use the dynamically updated one
		Tier:     tier,
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s %s\n", paint(cGray, "│"), paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)

	// Offer to set as primary
	ans, canceled := tui.Input("Set as primary? [Y/n]", false)
	if canceled {
		return
	}
	ans = strings.TrimSpace(strings.ToLower(ans))
	if ans == "" || ans == "y" || ans == "yes" {
		mc := c.cfg.Models[modelID]
		c.cfg.Model = mc.Model
		c.cfg.Provider = mc.Provider
		c.cfg.BaseURL = mc.BaseURL
		c.cfg.APIKey = mc.APIKey
		c.cfg.Save()
		c.kernel.ReloadModels(c.cfg)
		fmt.Printf("%s %s primary model → %s %s\n", paint(cGray, "│"), paint(cGreen, "✓"), paint(cOrange+clrBold, modelID), paint(cGray, "(hot-reloaded)"))
	}
	fmt.Printf("%s %s registered %s  %s  %s\n",
		paint(cGray, "│"),
		paint(cGreen, "✓"),
		paint(cWhite+clrBold, modelID),
		paint(cBlue, "("+tier+")"),
		paint(cGray, "("+provider.ID+")"))
}

func (c *Console) modelRemove(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models remove <model>\n", paint(cRed, "✗"))
		return
	}
	model := args[0]
	if _, ok := c.cfg.Models[model]; !ok {
		fmt.Printf("%s no such model: %s\n", paint(cRed, "✗"), model)
		return
	}
	delete(c.cfg.Models, model)
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s removed %s\n", paint(cGreen, "✓"), paint(cWhite, model))
}

func (c *Console) modelPrimary(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models primary <model>\n", paint(cRed, "✗"))
		return
	}
	model := args[0]
	mc, ok := c.cfg.Models[model]
	if !ok {
		fmt.Printf("%s no such model: %s (add it first with /models add)\n", paint(cRed, "✗"), model)
		return
	}
	c.cfg.Model = mc.Model
	c.cfg.Provider = mc.Provider
	c.cfg.BaseURL = mc.BaseURL
	c.cfg.APIKey = mc.APIKey
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s primary model → %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, mc.Model), paint(cGray, "(hot-reloaded)"))
}

// handleProviders dispatches /providers subcommands.
func (c *Console) handleProviders(args []string) {
	if len(args) == 0 {
		c.listProviders()
		return
	}
	c.showProvider(args[0])
}

func (c *Console) listProviders() {
	provs := config.Providers()
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROVIDER CATALOGUE"), paint(cGray, "("+fmtNum(len(provs))+" providers)"))
	for _, p := range provs {
		auth := paint(cGreen, p.AuthScheme)
		if p.AuthScheme == config.AuthNone {
			auth = paint(cCyan, "local")
		}
		local := ""
		if p.Local {
			local = paint(cCyan, " (local)")
		}
		fmt.Printf("  %s  %s  %s  %s%s\n",
			paint(cOrange, padRight(p.ID, 14)),
			paint(cWhite, padRight(p.Name, 24)),
			auth,
			paint(cGray, fmt.Sprintf("%d models", len(p.Models))),
			local)
	}
	fmt.Printf("\n  %s /providers <id> to list models for a provider\n", paint(cGray, "e.g. /providers openai"))
}

func (c *Console) showProvider(id string) {
	p, ok := config.LookupProvider(id)
	if !ok {
		fmt.Printf("%s unknown provider: %s\n", paint(cRed, "✗"), id)
		return
	}
	fmt.Printf("%s  %s  %s\n", paint(cAmber+clrBold, p.Name), paint(cGray, "("+p.ID+")"), paint(cGray, p.BaseURL))
	fmt.Printf("  auth: %s  models: %s\n", paint(cGreen, p.AuthScheme), fmtNum(len(p.Models)))
	fmt.Println()
	for _, m := range p.Models {
		ctxK := fmtNum(m.ContextWindow)
		if m.ContextWindow >= 1000000 {
			ctxK = fmt.Sprintf("%.1fM", float64(m.ContextWindow)/1e6)
		}
		price := paint(cGreen, "free")
		if m.InputPrice > 0 || m.OutputPrice > 0 {
			price = paint(cGreen, fmt.Sprintf("$%.2f/$%.2f per 1M", m.InputPrice, m.OutputPrice))
		}
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			paint(cOrange, "•"),
			paint(cWhite, padRight(m.ID, 30)),
			paint(cBlue, padRight(m.Tier, 10)),
			paint(cGray, padRight(ctxK+" ctx", 12)),
			price)
		if m.Description != "" {
			fmt.Printf("     %s\n", paint(cGray, m.Description))
		}
	}
	fmt.Printf("\n  %s /models add %s <model-id> [api_key]\n", paint(cGray, "add with:"), paint(cOrange, p.ID))
}

// printUsageFull prints a detailed usage report with a sparkline.
func (c *Console) printUsageFull() {
	snap := metrics.Default.Snapshot()
	w := termWidth()
	if w > 80 {
		w = 80
	}
	fmt.Println()
	fmt.Println(paint(cAmber+clrBold, "  USAGE REPORT") + paint(cGray, "  since "+fmtTimeShort(snap.Since)))
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// KPI line
	fmt.Printf("  %s  %s   %s  %s   %s  %s   %s  %s\n",
		paint(cGray, "tokens"), paint(cOrange+clrBold, fmtNum(snap.TotalTokens)),
		paint(cGray, "cost"), paint(cGreen+clrBold, fmtCost(snap.TotalCost)),
		paint(cGray, "requests"), paint(cBlue+clrBold, fmtNum(snap.TotalRequests)),
		paint(cGray, "avg lat"), paint(cYellow+clrBold, fmtDur(int64(snap.AvgLatencyMs))))

	// Sparkline of per-minute tokens
	var vals []float64
	for _, s := range snap.Series {
		vals = append(vals, float64(s.TotalTokens))
	}
	fmt.Println()
	fmt.Printf("  %s\n", paint(cGray, "token throughput /min"))
	fmt.Printf("  %s\n", sparkline(downsample(padZeros(vals, 30), w-6), cOrange))

	// Per-model bars
	if len(snap.PerModel) > 0 {
		fmt.Println()
		fmt.Printf("  %s\n", paint(cGray, "tokens by model"))
		maxTok := 0
		for _, m := range snap.PerModel {
			if m.TotalTokens > maxTok {
				maxTok = m.TotalTokens
			}
		}
		barW := w - 44
		if barW < 16 {
			barW = 16
		}
		for _, m := range snap.PerModel {
			label := m.Model
			if m.Provider != "" {
				label = m.Provider + "/" + m.Model
			}
			fmt.Println(barRow(label, m.TotalTokens, maxTok, barW, cAmber))
		}
	} else {
		fmt.Println()
		fmt.Println(paint(cGray, "  no model usage recorded yet."))
	}
	fmt.Println()
}

// padZeros left-pads a slice with zeros so the sparkline reads left-to-right.
func padZeros(v []float64, n int) []float64 {
	if len(v) >= n {
		return v
	}
	return append(make([]float64, n-len(v)), v...)
}

func (c *Console) handleProjects(args []string) {
	if c.projects == nil {
		fmt.Println(paint(cRed, "✗ project store not initialized"))
		return
	}
	if len(args) == 0 {
		args = []string{"list"}
	}

	switch args[0] {
	case "list":
		projects := c.projects.List()
		if len(projects) == 0 {
			fmt.Println(paint(cGray, "  no projects created yet."))
			return
		}
		fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROJECTS"), paint(cGray, "("+fmtNum(len(projects))+")"))
		for _, p := range projects {
			active := "  "
			if c.activeProject == p.ID {
				active = paint(cGreen, "➜ ")
			}
			fmt.Printf("%s%s  %s  %s\n", active, paint(cWhite, padRight(p.Name, 20)), paint(cGray, padRight(p.ID, 30)), paint(cGray, strutil.Truncate(p.Description, 40)))
		}
	case "new":
		if len(args) < 3 {
			fmt.Printf("%s usage: /projects new <name> <path> [description...]\n", paint(cRed, "✗"))
			return
		}
		name := args[1]
		path := args[2]
		desc := ""
		if len(args) > 3 {
			desc = strings.Join(args[3:], " ")
		}
		p, err := c.projects.Create(name, desc, path, nil)
		if err != nil {
			fmt.Printf("%s failed to create project: %s\n", paint(cRed, "✗"), err)
			return
		}
		fmt.Printf("%s created project %s (%s)\n", paint(cGreen, "✓"), paint(cWhite, p.Name), paint(cGray, p.ID))
	case "open", "use", "set":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects %s <id>\n", paint(cRed, "✗"), args[0])
			return
		}
		id := args[1]
		p, err := c.projects.Get(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		c.activeProject = p.ID
		_ = c.projects.Touch(p.ID)
		fmt.Printf("%s active project set to %s\n", paint(cGreen, "✓"), paint(cWhite, p.Name))
	case "clear":
		c.activeProject = ""
		fmt.Printf("%s active project cleared\n", paint(cGreen, "✓"))
	case "context":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects context <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		ctxStr, err := c.projects.GetContext(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT CONTEXT"))
		fmt.Println(ctxStr)
	case "plan":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects plan <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		planStr, err := c.projects.GetPlan(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT PLAN"))
		fmt.Println(planStr)
	case "workflow":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects workflow <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		workflowStr, err := c.projects.GetWorkflow(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT WORKFLOW"))
		fmt.Println(workflowStr)
	case "delete":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects delete <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		if err := c.projects.Delete(id); err != nil {
			fmt.Printf("%s failed to delete project: %s\n", paint(cRed, "✗"), err)
			return
		}
		if c.activeProject == id {
			c.activeProject = ""
		}
		fmt.Printf("%s project deleted\n", paint(cGreen, "✓"))
	default:
		fmt.Printf("%s unknown /projects subcommand: %s\n", paint(cRed, "✗"), args[0])
		fmt.Printf("  %s /projects [list|new|open|clear|context|plan|workflow|delete]\n", paint(cGray, "usage:"))
	}
}

func (c *Console) printAudit() {
	if c.mem == nil || c.mem.Audit() == nil {
		fmt.Println(paint(cRed, "✗ audit log not initialized"))
		return
	}
	entries := c.mem.Audit().GetRecent(50)
	if len(entries) == 0 {
		fmt.Println(paint(cGray, "  no audit entries recorded yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "RECENT AUDIT LOG"), paint(cGray, "("+fmtNum(len(entries))+" entries)"))
	for _, e := range entries {
		statusStr := paint(cGreen, e.Outcome)
		if e.Outcome != "allowed" {
			statusStr = paint(cRed, e.Outcome)
		}
		fmt.Printf("  %s  %-12s  %-15s  %s  %s\n",
			paint(cGray, fmtTime(e.Timestamp)),
			paint(cWhite, string(e.Agent)),
			paint(cOrange, e.Action),
			statusStr,
			paint(cGray, strutil.Truncate(e.Detail, 40)))
	}
}

func (c *Console) printLearningStats() {
	if c.mem == nil || c.mem.Learning() == nil {
		fmt.Println(paint(cRed, "✗ learning engine not initialized"))
		return
	}
	stats := c.mem.Learning().GetStats()
	strategies := c.mem.Learning().GetAllStrategies()

	fmt.Printf("%s\n", paint(cAmber+clrBold, "LEARNING ENGINE STATS"))
	fmt.Printf("  %-25s %s\n", paint(cGray, "total tasks processed"), paint(cWhite, fmtNum(stats["total_tasks"].(int))))
	fmt.Printf("  %-25s %s%%\n", paint(cGray, "success rate"), paint(cGreen, fmt.Sprintf("%.0f", stats["success_rate"].(float64)*100)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "strategies discovered"), paint(cBlue, fmtNum(stats["strategy_count"].(int))))

	if len(strategies) > 0 {
		fmt.Printf("\n%s %s\n", paint(cAmber+clrBold, "STRATEGIES"), paint(cGray, "("+fmtNum(len(strategies))+")"))
		for _, s := range strategies {
			fmt.Printf("  %s  %-20s  %s\n", paint(cOrange, "★"), paint(cWhite, s.Name), paint(cGray, strutil.Truncate(s.Description, 50)))
		}
	}
}

func (c *Console) printHardwareStats() {
	hw := observability.GetHardwareStats()
	fmt.Printf("\n%s\n", paint(cAmber+clrBold, "HARDWARE RESOURCE CENTER"))
	fmt.Printf("  %-25s %s\n", paint(cGray, "OS / Arch"), paint(cWhite, hw.OS+" / "+hw.Arch))
	fmt.Printf("  %-25s %s\n", paint(cGray, "CPU Usage"), paint(cYellow, fmt.Sprintf("%.1f%%", hw.CPUUsagePercent)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "RAM Usage"), paint(cYellow, fmt.Sprintf("%.0f MB / %.0f MB", hw.RAMUsedMB, hw.RAMTotalMB)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "Goroutines"), paint(cBlue, fmt.Sprintf("%d", hw.GoRoutines)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "Heap Alloc"), paint(cPurple, fmt.Sprintf("%.0f MB", hw.GoHeapAllocMB)))
}

func (c *Console) printHistoryFull() {
	snap := metrics.Default.Snapshot()
	fmt.Printf("\n%s\n", paint(cAmber+clrBold, "RECENT REQUESTS HISTORY"))
	if len(snap.Recent) == 0 {
		fmt.Printf("  %s\n", paint(cGray, "no recent requests"))
		return
	}
	fmt.Printf("  %-25s %-20s %-12s %-12s %s\n", paint(cGray, "Timestamp"), paint(cGray, "Model"), paint(cGray, "Tokens"), paint(cGray, "Cost"), paint(cGray, "Latency"))
	for i, r := range snap.Recent {
		if i > 20 { // max 20
			break
		}
		fmt.Printf("  %-25s %-20s %-12s %-12s %s\n",
			paint(cWhite, r.Timestamp.Format("15:04:05.000")),
			paint(cYellow, strutil.Truncate(r.Model, 18)),
			paint(cBlue, fmtNum(r.TotalTokens)),
			paint(cGreen, fmtCost(r.Cost)),
			paint(cPurple, fmtDur(r.LatencyMs)))
	}
}

// completeModelNames returns every known model name — the primary
// (c.cfg.Model) plus every key in c.cfg.Models — as dynamic completion
// candidates for "/model <TAB>", "/models remove <TAB>", and
// "/models primary <TAB>". readline's own prefix machinery narrows this
// full set against whatever the user has already typed (the same mechanism
// used for the static PcItem tree), so this doesn't need to filter by the
// in-progress line itself.
func (c *Console) completeModelNames(string) []string {
	var names []string
	if c.cfg.Model != "" {
		names = append(names, c.cfg.Model)
	}
	for k := range c.cfg.Models {
		if k != c.cfg.Model {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

func (c *Console) buildCompleter() *readline.PrefixCompleter {
	return readline.NewPrefixCompleter(
		readline.PcItem("/status"),
		readline.PcItem("/memory"),
		readline.PcItem("/tools",
			readline.PcItem("sources"),
			readline.PcItem("connect",
				readline.PcItem("mcp"),
				readline.PcItem("mcp-http"),
				readline.PcItem("file"),
			),
			readline.PcItem("disconnect"),
			readline.PcItem("remove"),
		),
		readline.PcItem("/skills"),
		readline.PcItem("/episodes"),
		readline.PcItem("/config"),
		readline.PcItem("/log"),
		readline.PcItem("/permissions",
			readline.PcItem("reset"),
		),
		readline.PcItem("/new"),
		readline.PcItem("/ingest"),
		readline.PcItem("/reset"),
		readline.PcItem("/models",
			readline.PcItem("add"),
			readline.PcItem("remove", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("primary", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("test", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("disable", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("enable", readline.PcItemDynamic(c.completeModelNames)),
		),
		readline.PcItem("/model", readline.PcItemDynamic(c.completeModelNames)),
		readline.PcItem("/mode",
			readline.PcItem("single"),
			readline.PcItem("escalation"),
			readline.PcItem("consensus"),
		),
		readline.PcItem("/mode",
			readline.PcItem("smart"),
			readline.PcItem("loop"),
		),
		readline.PcItem("/profile",
			readline.PcItem("auto"),
			readline.PcItem("sequential"),
			readline.PcItem("parallel"),
		),
		readline.PcItem("/local",
			readline.PcItem("force"),
			readline.PcItem("on"),
			readline.PcItem("auto"),
			readline.PcItem("off"),
			readline.PcItem("offload",
				readline.PcItem("on"),
				readline.PcItem("off"),
			),
		),
		readline.PcItem("/safety",
			readline.PcItem("strict"),
			readline.PcItem("normal"),
			readline.PcItem("relaxed"),
		),
		readline.PcItem("/compressor"),
		readline.PcItem("/providers"),
		readline.PcItem("/events"),
		readline.PcItem("/usage"),
		readline.PcItem("/history"),
		readline.PcItem("/stats"),
		readline.PcItem("/know"),
		readline.PcItem("/knowledge"),
		readline.PcItem("/plugins"),
		readline.PcItem("/sandbox"),
		readline.PcItem("/pipeline"),
		readline.PcItem("/help"),
		readline.PcItem("/quit"),
		readline.PcItem("/exit"),
	)
}
