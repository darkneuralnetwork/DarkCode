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
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/internal/jsonframe"
	"github.com/darkcode/internal/strutil"
)

// adapter describes how to start a debug adapter and how to talk to it.
//
// DAP itself is transport-agnostic and adapters split on the choice. debugpy
// speaks the protocol over its own stdin and stdout; Microsoft's js-debug has
// no stdio mode at all — `dapDebugServer.js` takes a port and listens on TCP.
// That is why the JavaScript path was written but never worked: not a missing
// binary, a transport this client did not have.
type adapter struct {
	argv []string
	// socket is true for an adapter that listens rather than reading stdin.
	socket bool
	// launch carries adapter-specific fields merged into the launch request.
	// DAP leaves the body of `launch` entirely to the adapter, so this is
	// where the two genuinely differ rather than something to unify.
	launch map[string]interface{}
}

// adapters maps a language to the DAP adapter that debugs it.
var adapters = map[string]adapter{
	"python": {argv: []string{"python3", "-m", "debugpy.adapter"}},
}

// listeningRe matches the line js-debug prints once its port is bound:
//
//	Debug server listening at 127.0.0.1:8123
//
// Reading the port back beats choosing one: picking a free port and passing it
// races with anything else on the machine binding it first, and the failure
// looks like a debugger that intermittently refuses to start.
var listeningRe = regexp.MustCompile(`listening at (?:.*:)?(\d+)`)

// dapHost is the interface a socket adapter binds, passed explicitly rather
// than left to default. js-debug defaults to "localhost", which resolves to
// ::1 on a dual-stack machine while this client dials 127.0.0.1 — the adapter
// starts, announces a port, and every connection is refused.
const dapHost = "127.0.0.1"

// jsLaunch is the launch configuration js-debug needs beyond the common
// fields. `type` is the important one: DAP says nothing about the body of a
// launch request, and js-debug uses this to pick which of its launchers to
// run. Without it the adapter accepts the request, starts nothing, and the
// program appears to run straight through without hitting a breakpoint.
var jsLaunch = map[string]interface{}{
	"type":    "pwa-node",
	"request": "launch",
	"name":    "darkcode",
}

// jsDebugAdapter locates Microsoft's js-debug DAP server.
//
// Checked in order of how deliberate each is: an explicit environment
// variable, then the location `darkcode` installs to, then a distribution
// package that provides a stdio-speaking wrapper on PATH.
func jsDebugAdapter() (adapter, bool) {
	if p := os.Getenv("DARKCODE_JS_DEBUG"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return adapter{argv: []string{"node", p, "0", dapHost}, socket: true, launch: jsLaunch}, true
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "share", "darkcode", "js-debug", "src", "dapDebugServer.js")
		if _, err := os.Stat(p); err == nil {
			return adapter{argv: []string{"node", p, "0", dapHost}, socket: true, launch: jsLaunch}, true
		}
	}
	// Some distributions ship a stdio wrapper under this name.
	if _, err := exec.LookPath("js-debug-adapter"); err == nil {
		return adapter{argv: []string{"js-debug-adapter"}, launch: jsLaunch}, true
	}
	return adapter{}, false
}

// adapterFor resolves the adapter for a language.
func adapterFor(lang string) (adapter, bool) {
	if lang == "javascript" {
		return jsDebugAdapter()
	}
	a, ok := adapters[lang]
	return a, ok
}

// dapRequestTimeout bounds one request. An adapter that stops answering must
// fail rather than hang the agent.
const dapRequestTimeout = 30 * time.Second

// dapSession is a running DAP adapter plus the debuggee it launched.
type dapSession struct {
	cmd   *exec.Cmd
	stdin interface{ Write([]byte) (int, error) }
	// conn is set for socket adapters and must be closed with the session;
	// the process alone exiting would leave the connection dangling.
	conn   io.ReadWriteCloser
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

	// addr is the adapter's listening address, needed to open the child
	// sessions js-debug asks for.
	addr string
	// children carries configurations from inbound startDebugging requests.
	children chan map[string]interface{}
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
	ad, ok := adapterFor(lang)
	if !ok {
		if lang == "javascript" {
			return nil, fmt.Errorf("no JavaScript debug adapter found — install Microsoft's js-debug " +
				"(https://github.com/microsoft/vscode-js-debug/releases, the js-debug-dap tarball) into " +
				"~/.local/share/darkcode/js-debug, or point DARKCODE_JS_DEBUG at its src/dapDebugServer.js")
		}
		return nil, fmt.Errorf("no debug adapter configured for %s", lang)
	}
	if _, err := exec.LookPath(ad.argv[0]); err != nil {
		return nil, fmt.Errorf("%s is not installed — needed to debug %s", ad.argv[0], lang)
	}

	cmd := exec.CommandContext(ctx, ad.argv[0], ad.argv[1:]...)
	cmd.Dir = opts.Dir

	var (
		conn   io.ReadWriteCloser
		stdin  interface{ Write([]byte) (int, error) }
		stdout io.Reader
		addr   string
	)
	if ad.socket {
		// The adapter prints the port it bound; read it back rather than
		// choosing one, so nothing races us to it.
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("starting %s: %w", ad.argv[0], err)
		}
		port, err := awaitPort(out)
		if err != nil {
			_ = cmd.Process.Kill()
			return nil, err
		}
		addr = net.JoinHostPort(dapHost, port)
		c, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("connecting to the debug adapter on port %s: %w", port, err)
		}
		conn, stdin, stdout = c, c, c
	} else {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("starting %s: %w", ad.argv[0], err)
		}
		stdin, stdout = in, out
	}

	s := &dapSession{
		cmd: cmd, stdin: stdin, conn: conn, addr: addr,
		pending:     map[int64]chan dapResponse{},
		stopped:     make(chan dapStopped, 8),
		initialized: make(chan struct{}),
		exited:      make(chan int, 1),
		children:    make(chan map[string]interface{}, 4),
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
			Seq        int64           `json:"seq"`
			RequestSeq int64           `json:"request_seq"`
			Success    bool            `json:"success"`
			Message    string          `json:"message"`
			Event      string          `json:"event"`
			Command    string          `json:"command"`
			Arguments  json.RawMessage `json:"arguments"`
			Body       json.RawMessage `json:"body"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}

		// DAP failures are almost always a disagreement about the protocol
		// rather than a bug in either side, and they are invisible without
		// seeing the wire. This trace is what turned "no breakpoint hits" into
		// a startDebugging request nobody was answering.
		if os.Getenv("DARKCODE_DAP_TRACE") != "" {
			fmt.Fprintf(os.Stderr, "[dap<-] %s\n", strutil.Truncate(string(body), 300))
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
		case "request":
			s.handleReverseRequest(msg.Seq, msg.Command, msg.Arguments)
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
	// A socket adapter outlives its connection: killing the process without
	// closing the connection leaves readLoop blocked on a socket nothing will
	// ever write to again.
	if s.conn != nil {
		_ = s.conn.Close()
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
	if ad, ok := adapterFor(lang); ok {
		for k, v := range ad.launch {
			launchArgs[k] = v
		}
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
	// pending records breakpoints an adapter has accepted but not yet bound.
	var pending []string
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
				Reason   string `json:"reason"`
			} `json:"breakpoints"`
		}
		_ = json.Unmarshal(body, &set)
		for i, bp := range set.Breakpoints {
			line := 0
			if i < len(lines) {
				line = lines[i]
			}
			switch {
			case bp.Verified:
				verified++
			case provisional(bp.Reason, bp.Message):
				// Adapters differ on when a breakpoint binds. debugpy resolves
				// it at set time; js-debug answers "pending" or
				// "provisionalBreakpoint" because the script has not been
				// loaded yet, and binds once it is. Treating that as a failure
				// rejected every JavaScript breakpoint before the program had a
				// chance to start. Whether it really bound is decided by
				// whether it is hit, which is the honest test either way.
				verified++
				pending = append(pending, fmt.Sprintf("%s:%d", file, line))
			default:
				report.Unbound = append(report.Unbound,
					fmt.Sprintf("%s:%d — %s", file, line, strutil.NonEmpty(bp.Message, "not verified by the adapter")))
			}
		}
	}
	if verified == 0 {
		return report, fmt.Errorf("no breakpoint could be set: %s", strings.Join(report.Unbound, "; "))
	}
	_ = pending // accepted; whether each really bound is decided by the hits

	if _, err := s.request(ctx, "configurationDone", map[string]interface{}{}); err != nil {
		return report, err
	}

	// js-debug is a multi-session debugger: the root session starts the
	// process and only then asks, via a startDebugging reverse request, for a
	// second session covering the target it created. Breakpoints and stops
	// belong to that child, so the same breakpoints are set again on it and
	// everything after this point is addressed to it.
	//
	// The request arrives after configurationDone, which is why this waits
	// here rather than earlier. Single-session adapters (debugpy) never send
	// it and fall through on the timeout.
	target := s
	select {
	case config := <-s.children:
		child, err := s.attachChild(ctx, config)
		if err != nil {
			return report, err
		}
		defer child.closeChild()
		if err := child.setBreakpoints(ctx, byFile); err != nil {
			return report, err
		}
		if _, err := child.request(ctx, "configurationDone", map[string]interface{}{}); err != nil {
			return report, err
		}
		target = child
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return report, ctx.Err()
	}

	for len(report.Observations) < maxHits {
		select {
		case st := <-target.stopped:
			obs, err := target.observe(ctx, st.ThreadID, exprs)
			if err != nil {
				return report, err
			}
			report.Observations = append(report.Observations, obs)
			if _, err := target.request(ctx, "continue", map[string]interface{}{"threadId": st.ThreadID}); err != nil {
				return report, nil // the process most likely finished
			}
		case code := <-target.exited:
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

// awaitPort reads the adapter's startup output until it announces its port.
//
// The scan stops at the first match rather than draining the pipe, because the
// adapter keeps logging afterwards and a reader that consumed it all would
// block forever waiting for an EOF that only arrives when the debugger exits.
func awaitPort(out io.Reader) (string, error) {
	type found struct {
		port string
		err  error
	}
	ch := make(chan found, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if m := listeningRe.FindStringSubmatch(scanner.Text()); m != nil {
				ch <- found{port: m[1]}
				return
			}
		}
		ch <- found{err: fmt.Errorf("the debug adapter exited before announcing a port")}
	}()

	select {
	case f := <-ch:
		return f.port, f.err
	case <-time.After(20 * time.Second):
		return "", fmt.Errorf("the debug adapter did not announce a port within 20s")
	}
}

// provisional reports whether an adapter has accepted a breakpoint but not yet
// bound it, which is a normal state rather than a failure.
//
// js-debug answers "provisionalBreakpoint" or "pending" for a breakpoint in a
// script it has not loaded, and binds it when the script arrives. debugpy has
// no such state. The distinction matters because "not verified" and "not
// verified *yet*" are the same field.
func provisional(reason, message string) bool {
	for _, s := range []string{reason, message} {
		l := strings.ToLower(s)
		if strings.Contains(l, "provisional") || strings.Contains(l, "pending") {
			return true
		}
	}
	return false
}

// handleReverseRequest answers a request the adapter sends to *us*.
//
// js-debug is a multi-session debugger: the root session launches the process
// and then asks the client, via a `startDebugging` reverse request, to open a
// second session for the target it just created. The program's breakpoints and
// stops belong to that child, not to the root — which is why a client that
// ignores inbound requests sees a launch succeed, a breakpoint accepted, and
// then nothing at all.
func (s *dapSession) handleReverseRequest(seq int64, command string, args json.RawMessage) {
	switch command {
	case "startDebugging":
		var a struct {
			Configuration map[string]interface{} `json:"configuration"`
		}
		_ = json.Unmarshal(args, &a)
		select {
		case s.children <- a.Configuration:
		default: // more targets than we can follow; the first is the program
		}
		s.respond(seq, command, true, nil)
	case "runInTerminal":
		// Never requested: initialize advertises no support for it. Refusing
		// explicitly beats leaving the adapter waiting.
		s.respond(seq, command, false, nil)
	default:
		s.respond(seq, command, true, nil)
	}
}

// respond answers a reverse request. An unanswered one leaves js-debug waiting
// forever for a session it asked for.
func (s *dapSession) respond(seq int64, command string, success bool, body interface{}) {
	msg := map[string]interface{}{
		"seq":         atomic.AddInt64(&s.seq, 1),
		"type":        "response",
		"request_seq": seq,
		"command":     command,
		"success":     success,
	}
	if body != nil {
		msg["body"] = body
	}
	blob, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = jsonframe.Write(s.stdin, blob)
}

// attachChild opens the session js-debug asked for and prepares it to receive
// breakpoints. The returned session owns its own connection and must be closed.
func (s *dapSession) attachChild(ctx context.Context, config map[string]interface{}) (*dapSession, error) {
	if s.addr == "" {
		return nil, fmt.Errorf("no adapter address to open a child session on")
	}
	conn, err := net.DialTimeout("tcp", s.addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting the child debug session: %w", err)
	}
	child := &dapSession{
		stdin: conn, conn: conn, addr: s.addr, cmd: s.cmd,
		pending:     map[int64]chan dapResponse{},
		stopped:     make(chan dapStopped, 8),
		initialized: make(chan struct{}),
		exited:      make(chan int, 1),
		children:    make(chan map[string]interface{}, 4),
	}
	go child.readLoop(bufio.NewReaderSize(conn, 64*1024))

	if _, err := child.request(ctx, "initialize", map[string]interface{}{
		"adapterID": "darkcode", "clientID": "darkcode",
		"linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path",
		"supportsRunInTerminalRequest": false,
	}); err != nil {
		child.closeChild()
		return nil, err
	}
	// The child's launch body is the configuration the parent handed us
	// verbatim; __pendingTargetId inside it is what binds this session to the
	// process the root already started.
	go func() { _, _ = child.request(ctx, "attach", config) }()

	select {
	case <-child.initialized:
	case <-time.After(dapRequestTimeout):
		child.closeChild()
		return nil, fmt.Errorf("the child debug session never initialised")
	case <-ctx.Done():
		child.closeChild()
		return nil, ctx.Err()
	}
	return child, nil
}

// closeChild tears down a child session without touching the shared process.
func (s *dapSession) closeChild() {
	if s.closed.Swap(true) {
		return
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// setBreakpoints installs breakpoints on this session, ignoring verification:
// the caller has already decided the set is usable, and a child session
// re-registering them reports the same provisional state the root did.
func (s *dapSession) setBreakpoints(ctx context.Context, byFile map[string][]int) error {
	for file, lines := range byFile {
		var wanted []map[string]interface{}
		for _, line := range lines {
			wanted = append(wanted, map[string]interface{}{"line": line})
		}
		if _, err := s.request(ctx, "setBreakpoints", map[string]interface{}{
			"source":      map[string]interface{}{"path": file},
			"breakpoints": wanted,
		}); err != nil {
			return fmt.Errorf("setting breakpoints in %s: %w", file, err)
		}
	}
	return nil
}
