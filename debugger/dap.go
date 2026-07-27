package debugger

// dap.go — a Debug Adapter Protocol client.
//
// Delve has its own JSON-RPC API, so the Go path talks to it directly. Every
// other language worth debugging speaks DAP instead — debugpy for Python,
// js-debug for Node — and they all speak the *same* DAP. So this is one client
// rather than one integration per language: adding a language becomes a table
// entry naming its adapter, not another protocol implementation.
//
// DAP frames messages exactly like LSP (see internal/jsonframe) but the
// envelope differs: every message carries a sequence number and is a request,
// a response, or an unsolicited event. Breakpoint hits arrive as events, so
// the client has to listen rather than poll.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/internal/jsonframe"
)

// adapters maps a language to the DAP adapter that debugs it.
var adapters = map[string][]string{
	"python": {"python3", "-m", "debugpy.adapter"},
	// js-debug ships as a Node script; its DAP server mode is what VS Code
	// drives. Absent on most machines, which is handled like any missing tool.
	"javascript": {"js-debug-adapter"},
}

// dapRequestTimeout bounds one request. An adapter that stops answering must
// fail rather than hang the agent.
const dapRequestTimeout = 30 * time.Second

// dapSession is a running DAP adapter plus the debuggee it launched.
type dapSession struct {
	cmd    *exec.Cmd
	stdin  interface{ Write([]byte) (int, error) }
	seq    int64
	closed atomic.Bool

	mu      sync.Mutex
	pending map[int64]chan dapResponse
	// stopped receives every `stopped` event; buffered so an event that
	// arrives before anyone waits is not lost.
	stopped chan dapStopped
	// initialized signals the adapter is ready for breakpoints.
	initialized chan struct{}
	initOnce    sync.Once
	// exited records the debuggee finishing, which ends the inspection loop.
	exited   chan int
	exitOnce sync.Once
}

type dapResponse struct {
	Success bool
	Message string
	Body    json.RawMessage
}

type dapStopped struct {
	Reason   string `json:"reason"`
	ThreadID int    `json:"threadId"`
}

// launchDAP starts an adapter for a language and completes the handshake up to
// the point where breakpoints can be set.
func launchDAP(ctx context.Context, lang string, opts Options) (*dapSession, error) {
	argv, ok := adapters[lang]
	if !ok {
		return nil, fmt.Errorf("no debug adapter configured for %s", lang)
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, fmt.Errorf("%s is not installed — needed to debug %s", argv[0], lang)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}

	s := &dapSession{
		cmd: cmd, stdin: stdin,
		pending:     map[int64]chan dapResponse{},
		stopped:     make(chan dapStopped, 8),
		initialized: make(chan struct{}),
		exited:      make(chan int, 1),
	}
	go s.readLoop(bufio.NewReaderSize(stdout, 64*1024))

	if _, err := s.request(ctx, "initialize", map[string]interface{}{
		"adapterID":                    "darkcode",
		"clientID":                     "darkcode",
		"linesStartAt1":                true,
		"columnsStartAt1":              true,
		"pathFormat":                   "path",
		"supportsRunInTerminalRequest": false,
	}); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// readLoop dispatches responses to their caller and routes events.
func (s *dapSession) readLoop(r *bufio.Reader) {
	defer s.markExited(-1)
	for {
		body, err := jsonframe.Read(r)
		if err != nil {
			return
		}
		var msg struct {
			Type       string          `json:"type"`
			RequestSeq int64           `json:"request_seq"`
			Success    bool            `json:"success"`
			Message    string          `json:"message"`
			Event      string          `json:"event"`
			Body       json.RawMessage `json:"body"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "response":
			s.mu.Lock()
			ch, ok := s.pending[msg.RequestSeq]
			delete(s.pending, msg.RequestSeq)
			s.mu.Unlock()
			if ok {
				ch <- dapResponse{Success: msg.Success, Message: msg.Message, Body: msg.Body}
			}
		case "event":
			s.handleEvent(msg.Event, msg.Body)
		}
	}
}

func (s *dapSession) handleEvent(event string, body json.RawMessage) {
	switch event {
	case "initialized":
		// Fires once; closing twice would panic.
		s.initOnce.Do(func() { close(s.initialized) })
	case "stopped":
		var st dapStopped
		if json.Unmarshal(body, &st) == nil {
			select {
			case s.stopped <- st:
			default: // a slow consumer must not block the read loop
			}
		}
	case "terminated", "exited":
		var e struct {
			ExitCode int `json:"exitCode"`
		}
		_ = json.Unmarshal(body, &e)
		s.markExited(e.ExitCode)
	}
}

func (s *dapSession) markExited(code int) {
	s.exitOnce.Do(func() { s.exited <- code; close(s.exited) })
}

// request sends a DAP request and waits for its response.
func (s *dapSession) request(ctx context.Context, command string, args interface{}) (json.RawMessage, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("debug adapter is closed")
	}
	seq := atomic.AddInt64(&s.seq, 1)
	ch := make(chan dapResponse, 1)

	s.mu.Lock()
	s.pending[seq] = ch
	s.mu.Unlock()

	body, err := json.Marshal(map[string]interface{}{
		"seq": seq, "type": "request", "command": command, "arguments": args,
	})
	if err != nil {
		return nil, err
	}
	if err := jsonframe.Write(s.stdin, body); err != nil {
		s.mu.Lock()
		delete(s.pending, seq)
		s.mu.Unlock()
		return nil, err
	}

	timeout := time.NewTimer(dapRequestTimeout)
	defer timeout.Stop()
	select {
	case resp := <-ch:
		if !resp.Success {
			return nil, fmt.Errorf("%s: %s", command, strings.TrimSpace(resp.Message))
		}
		return resp.Body, nil
	case <-timeout.C:
		s.mu.Lock()
		delete(s.pending, seq)
		s.mu.Unlock()
		return nil, fmt.Errorf("%s: adapter did not respond within %s", command, dapRequestTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *dapSession) close() {
	// Send disconnect BEFORE marking closed: request() refuses once the flag
	// is set, so flipping it first would silently skip the graceful shutdown
	// and leave the debuggee to be killed instead.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, _ = s.request(ctx, "disconnect", map[string]interface{}{"terminateDebuggee": true})
	cancel()
	if s.closed.Swap(true) {
		return
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
}

// inspectDAP runs the full launch/break/observe cycle against a DAP adapter,
// producing the same Report as the delve path so callers never branch on
// language.
func inspectDAP(ctx context.Context, lang string, opts Options, breakpoints []Breakpoint, exprs []string) (*Report, error) {
	s, err := launchDAP(ctx, lang, opts)
	if err != nil {
		return nil, err
	}
	defer s.close()

	report := &Report{Target: opts.Dir + " (" + lang + ")"}

	// The launch request is answered only after the adapter is configured, so
	// it is issued before waiting for the `initialized` event rather than
	// after — the reverse order deadlocks.
	launchArgs := map[string]interface{}{
		"program": opts.Program, "cwd": opts.Dir,
		"console": "internalConsole", "justMyCode": false, "stopOnEntry": false,
	}
	// Omit args entirely when there are none: a nil slice marshals to null,
	// and debugpy rejects that rather than treating it as absent.
	if len(opts.Args) > 0 {
		launchArgs["args"] = opts.Args
	}
	launchErr := make(chan error, 1)
	go func() {
		_, err := s.request(ctx, "launch", launchArgs)
		launchErr <- err
	}()

	select {
	case <-s.initialized:
	case err := <-launchErr:
		if err != nil {
			return nil, err
		}
	case <-time.After(dapRequestTimeout):
		return nil, fmt.Errorf("adapter never reported it was initialized")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// DAP sets breakpoints per source file, replacing any previous set, so
	// they are grouped rather than sent one at a time.
	byFile := map[string][]int{}
	for _, bp := range breakpoints {
		byFile[bp.File] = append(byFile[bp.File], bp.Line)
	}
	verified := 0
	for file, lines := range byFile {
		var wanted []map[string]interface{}
		for _, line := range lines {
			wanted = append(wanted, map[string]interface{}{"line": line})
		}
		body, err := s.request(ctx, "setBreakpoints", map[string]interface{}{
			"source":      map[string]interface{}{"path": file},
			"breakpoints": wanted,
		})
		if err != nil {
			report.Unbound = append(report.Unbound, fmt.Sprintf("%s — %v", file, err))
			continue
		}
		var set struct {
			Breakpoints []struct {
				Verified bool   `json:"verified"`
				Line     int    `json:"line"`
				Message  string `json:"message"`
			} `json:"breakpoints"`
		}
		_ = json.Unmarshal(body, &set)
		for i, bp := range set.Breakpoints {
			if bp.Verified {
				verified++
				continue
			}
			line := 0
			if i < len(lines) {
				line = lines[i]
			}
			report.Unbound = append(report.Unbound,
				fmt.Sprintf("%s:%d — %s", file, line, nonEmpty(bp.Message, "not verified by the adapter")))
		}
	}
	if verified == 0 {
		return report, fmt.Errorf("no breakpoint could be set: %s", strings.Join(report.Unbound, "; "))
	}

	if _, err := s.request(ctx, "configurationDone", map[string]interface{}{}); err != nil {
		return report, err
	}

	for len(report.Observations) < maxHits {
		select {
		case st := <-s.stopped:
			obs, err := s.observe(ctx, st.ThreadID, exprs)
			if err != nil {
				return report, err
			}
			report.Observations = append(report.Observations, obs)
			if _, err := s.request(ctx, "continue", map[string]interface{}{"threadId": st.ThreadID}); err != nil {
				return report, nil // the process most likely finished
			}
		case code := <-s.exited:
			report.Exited = true
			if code >= 0 {
				report.ExitStatus = code
			}
			return report, nil
		case <-time.After(dapRequestTimeout):
			return report, nil // nothing more is coming
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	return report, nil
}

// observe collects the frame, locals, expressions and stack at one stop.
func (s *dapSession) observe(ctx context.Context, threadID int, exprs []string) (Observation, error) {
	var obs Observation

	body, err := s.request(ctx, "stackTrace", map[string]interface{}{"threadId": threadID, "levels": 8})
	if err != nil {
		return obs, err
	}
	var trace struct {
		StackFrames []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Line   int    `json:"line"`
			Source *struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"stackFrames"`
	}
	if json.Unmarshal(body, &trace) != nil || len(trace.StackFrames) == 0 {
		return obs, fmt.Errorf("stackTrace returned no frames")
	}
	top := trace.StackFrames[0]
	obs.Line, obs.Function = top.Line, top.Name
	if top.Source != nil {
		obs.File = top.Source.Path
	}
	for _, f := range trace.StackFrames {
		frame := Frame{Function: f.Name, Line: f.Line}
		if f.Source != nil {
			frame.File = f.Source.Path
		}
		obs.Stack = append(obs.Stack, frame)
	}

	obs.Locals = s.locals(ctx, top.ID)
	for _, expr := range exprs {
		v := Variable{Name: expr}
		body, err := s.request(ctx, "evaluate", map[string]interface{}{
			"expression": expr, "frameId": top.ID, "context": "watch",
		})
		if err != nil {
			v.Value = "<" + err.Error() + ">"
		} else {
			var res struct {
				Result string `json:"result"`
				Type   string `json:"type"`
			}
			_ = json.Unmarshal(body, &res)
			v.Value, v.Type = res.Result, res.Type
		}
		obs.Expressions = append(obs.Expressions, v)
	}
	return obs, nil
}

// locals reads the local scope's variables. A failure here degrades the
// observation rather than failing the run — the stack and expressions are
// still worth returning.
func (s *dapSession) locals(ctx context.Context, frameID int) []Variable {
	body, err := s.request(ctx, "scopes", map[string]interface{}{"frameId": frameID})
	if err != nil {
		return nil
	}
	var scopes struct {
		Scopes []struct {
			Name               string `json:"name"`
			VariablesReference int    `json:"variablesReference"`
		} `json:"scopes"`
	}
	if json.Unmarshal(body, &scopes) != nil {
		return nil
	}

	var out []Variable
	for _, sc := range scopes.Scopes {
		// Globals and builtins are noise; the local frame is the question.
		if !strings.Contains(strings.ToLower(sc.Name), "local") || sc.VariablesReference == 0 {
			continue
		}
		body, err := s.request(ctx, "variables", map[string]interface{}{"variablesReference": sc.VariablesReference})
		if err != nil {
			continue
		}
		var vars struct {
			Variables []struct {
				Name, Value, Type string
			} `json:"variables"`
		}
		if json.Unmarshal(body, &vars) != nil {
			continue
		}
		for _, v := range vars.Variables {
			out = append(out, Variable{Name: v.Name, Value: v.Value, Type: v.Type})
		}
	}
	return out
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
