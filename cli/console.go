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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darkcode/internal/strutil"

	"github.com/chzyer/readline"

	"github.com/darkcode/attach"
	"github.com/darkcode/checkpoint"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/project"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
	"github.com/darkcode/verb"
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
	ckpt          *checkpoint.Manager
	activeProject string

	// stickyVerb is the strategy every message uses until the user says
	// otherwise ("" = none, and escalation decides per message).
	//
	// This replaced a separate chat/build/loop vocabulary. Three vocabularies
	// for "how should this run" — the routing mode, the chat mode, and the
	// verbs — meant the console could disagree with itself, and did: picking
	// Loop printed a note telling you to go and enable it somewhere else.
	stickyVerb string
	// pendingVerb is the strategy a one-shot verb selected for the NEXT
	// message only. Nil most of the time; cleared the moment it is used, so a
	// verb can never silently persist into a later request the way a sticky
	// mode does.
	pendingVerb *strategy
	// brain mirrors the GUI's Brain selector: "auto" (local-first, escalate),
	// "local" (offline — pin to the local model), or "cloud". Passed to
	// ApplyRequestOverrides per query.
	brain string

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
		brain:         "auto", // local-first, escalate to cloud for hard tasks
		streamEv:      true,   // minimal spinner on by default
	}

	c.ckpt = kernel.Checkpoints()

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
			paint(cGray, "  ·  mode: "+mode+c.stickyNote()+"  ·  /help for commands  ·  /gui to return"))
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
	readReq := make(chan string)       // prompt to use for the next Readline()
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

	// A strategy verb carries a task on the same line: "/loop add retries".
	// It is checked before the command table because it is not a command — it
	// selects how THIS message runs and then runs it, which is the whole point
	// of a verb over a setting. A bare "/loop" falls through to the command
	// table, which prints help rather than silently arming a mode.
	if !strings.Contains(input, "\n") {
		if st, task, ok := splitVerb(input); ok {
			v := st
			c.pendingVerb = &v
			query, atts := attach.ParseRefs(task)
			c.runQuery(ctx, query, atts)
			return false, false
		}
	}

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
	if c.cfg.Model == "" && !c.cfg.LocalEnabled() {
		fmt.Printf("%s please select a model or initialise the local llm\n", paint(cRed, "✗"))
		return
	}
	origQuery := query

	// (Removed) undocumented "auto" routing-mode classifier: "auto" is not a
	// valid kernel routing mode (only single|escalation|consensus), it was
	// unreachable via --mode or /routing, and it fired a hidden LLM call on
	// every query. Use the /project create command to create a project.

	if c.activeProject != "" && c.projects != nil {
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

	if c.activeProject != "" && c.projects != nil {
		workflow, _ := c.projects.GetWorkflow(c.activeProject)
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

	// Strategy for THIS message, in precedence order: a one-shot verb on the
	// line, then a sticky verb from /always, then escalation. The same order
	// the web UI uses — the console reads the same table and the same rung
	// chooser, so "what will this do" has one answer rather than three.
	st := c.strategyForMessage(resolvedQuery)
	loopOverride, toolsOverride := st.Loop, st.Tools
	modeOverride, planOverride := st.Mode, st.Plan
	restoreOverrides := c.kernel.ApplyRequestOverrides(modeOverride, "", loopOverride, toolsOverride, c.brain)
	defer restoreOverrides()
	restorePlan := c.kernel.ApplyPlanOverride(planOverride)
	defer restorePlan()

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
			client := llm.WrapCloud(llm.NewClient(c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Model), c.cfg.Provider, c.cfg.Model)
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

// strategyForMessage resolves what this one message should do.
//
// Precedence is one-shot verb, then sticky verb, then escalation — narrowest
// intent first. The escalation fallback is what keeps the console and the web
// UI in step: neither asks the user to pick up front, and both climb on the
// same signals when the cheap attempt turns out not to be enough.
func (c *Console) strategyForMessage(query string) verb.Strategy {
	if v := c.pendingVerb; v != nil {
		c.pendingVerb = nil // one shot: consumed by this message and no other
		return *v
	}
	if c.stickyVerb != "" {
		if s, ok := verb.Lookup(c.stickyVerb); ok {
			return s
		}
	}
	effort, why := router.EntryEffort(query)
	if v := effort.Verb(); v != "" {
		fmt.Println(paint(cGray, "  ↳ "+why+" — same as /"+v))
	}
	return verb.ForEffort(effort)
}

// stickyNote renders the active /always verb for the status line, or nothing
// when escalation is deciding.
func (c *Console) stickyNote() string {
	if c.stickyVerb == "" {
		return ""
	}
	return "  ·  always: /" + c.stickyVerb
}
