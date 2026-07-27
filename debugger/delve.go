// Package debugger drives a real debugger so the agent can read runtime
// values instead of guessing at them.
//
// The usual agent loop for "why is this wrong" is to add print statements,
// re-run, read the output, and delete the prints — three tool calls and a
// dirty working tree to learn one value. A debugger answers the same question
// in one call, without touching the source.
//
// Delve exposes a headless JSON-RPC API, and Go's standard library speaks that
// protocol (net/rpc/jsonrpc), so this needs no dependency.
//
// One constraint drives the design: breakpoints only bind against an
// unoptimised build. `dlv debug` and `dlv test` compile with `-gcflags=all=-N
// -l`; pointing delve at an already-built binary silently fails to bind every
// breakpoint because the compiler inlined the function away. This package
// therefore always builds through delve and never attaches to a prebuilt
// binary.
package debugger

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// launchTimeout bounds how long we wait for delve to compile the target and
// start listening. A cold build of a large package is not fast.
const launchTimeout = 120 * time.Second

// Options configures a debug session.
type Options struct {
	// Dir is the package directory to build and debug.
	Dir string
	// Test debugs the package's test binary rather than its main function.
	Test bool
	// Run filters which tests execute, like `go test -run`. Ignored unless Test.
	Run string
	// Args are passed to the debugged program.
	Args []string
}

// Breakpoint is a location to stop at.
type Breakpoint struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// Function is filled in by delve once the breakpoint binds.
	Function string `json:"function,omitempty"`
	ID       int    `json:"id,omitempty"`
}

// Variable is one value observed at a stop.
type Variable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// Frame is one level of a stack trace.
type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// Stop describes where execution paused.
type Stop struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	GoroutineID int    `json:"goroutine_id"`
	Exited      bool   `json:"exited"`
	ExitStatus  int    `json:"exit_status"`
}

// Session is a running delve session.
type Session struct {
	cmd    *exec.Cmd
	client *rpc.Client

	mu     sync.Mutex
	closed bool
}

// listenRE extracts the address from delve's startup banner. Delve is asked to
// bind port 0 and then reports what it got, which avoids racing another
// process for a port we picked ourselves.
var listenRE = regexp.MustCompile(`API server listening at:\s*(\S+)`)

// Launch builds the target through delve and connects to it.
func Launch(ctx context.Context, opts Options) (*Session, error) {
	if _, err := exec.LookPath("dlv"); err != nil {
		return nil, fmt.Errorf("delve is not installed — `go install github.com/go-delve/delve/cmd/dlv@latest`")
	}

	mode := "debug"
	if opts.Test {
		mode = "test"
	}
	argv := []string{
		"--headless", "--listen=127.0.0.1:0", "--api-version=2",
		"--accept-multiclient", mode, ".",
	}
	// Everything after `--` belongs to the debugged program.
	var programArgs []string
	if opts.Test && opts.Run != "" {
		programArgs = append(programArgs, "-test.run", opts.Run)
	}
	programArgs = append(programArgs, opts.Args...)
	if len(programArgs) > 0 {
		argv = append(argv, "--")
		argv = append(argv, programArgs...)
	}

	cmd := exec.CommandContext(ctx, "dlv", argv...)
	cmd.Dir = opts.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // delve reports build failures on stderr; keep them together
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting delve: %w", err)
	}

	addr, banner, err := awaitListener(ctx, stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// The banner carries the compiler error when the target doesn't build,
		// which is the actual thing the caller needs to see.
		return nil, fmt.Errorf("delve did not start: %w\n%s", err, strings.TrimSpace(banner))
	}

	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("connecting to delve at %s: %w", addr, err)
	}
	return &Session{cmd: cmd, client: jsonrpc.NewClient(conn)}, nil
}

// awaitListener reads delve's output until it announces its address, draining
// the rest in the background so a chatty target can never block on a full pipe.
func awaitListener(ctx context.Context, stdout io.ReadCloser) (addr, banner string, err error) {
	type result struct {
		addr, banner string
		err          error
	}
	done := make(chan result, 1)

	go func() {
		var seen strings.Builder
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			seen.WriteString(line)
			seen.WriteByte('\n')
			if m := listenRE.FindStringSubmatch(line); m != nil {
				done <- result{addr: m[1], banner: seen.String()}
				// Keep draining so delve never blocks writing to the pipe.
				for scanner.Scan() {
				}
				return
			}
		}
		done <- result{banner: seen.String(), err: fmt.Errorf("delve exited before listening")}
	}()

	select {
	case r := <-done:
		return r.addr, r.banner, r.err
	case <-time.After(launchTimeout):
		return "", "", fmt.Errorf("timed out after %s waiting for delve to build and listen", launchTimeout)
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// Breakpoint sets a breakpoint. Delve binds by file and line; a line with no
// executable statement is refused rather than silently ignored, and that error
// is worth surfacing verbatim because the usual cause is an optimised build or
// a blank line.
func (s *Session) Breakpoint(file string, line int) (Breakpoint, error) {
	var out struct {
		Breakpoint struct {
			ID           int    `json:"id"`
			File         string `json:"file"`
			Line         int    `json:"line"`
			FunctionName string `json:"functionName"`
		}
	}
	in := map[string]interface{}{
		"Breakpoint": map[string]interface{}{"file": file, "line": line},
	}
	if err := s.call("RPCServer.CreateBreakpoint", in, &out); err != nil {
		return Breakpoint{}, err
	}
	return Breakpoint{
		ID: out.Breakpoint.ID, File: out.Breakpoint.File,
		Line: out.Breakpoint.Line, Function: out.Breakpoint.FunctionName,
	}, nil
}

// debuggerState is delve's reply shape for a command.
type debuggerState struct {
	State struct {
		CurrentThread *struct {
			File        string `json:"file"`
			Line        int    `json:"line"`
			GoroutineID int    `json:"goroutineID"`
		} `json:"currentThread"`
		Exited     bool `json:"exited"`
		ExitStatus int  `json:"exitStatus"`
	}
}

// Continue resumes execution until the next breakpoint or exit.
func (s *Session) Continue() (Stop, error) {
	var out debuggerState
	if err := s.call("RPCServer.Command", map[string]interface{}{"name": "continue"}, &out); err != nil {
		return Stop{}, err
	}
	stop := Stop{Exited: out.State.Exited, ExitStatus: out.State.ExitStatus}
	if t := out.State.CurrentThread; t != nil {
		stop.File, stop.Line, stop.GoroutineID = t.File, t.Line, t.GoroutineID
	}
	return stop, nil
}

// loadConfig bounds how much of a value delve renders. Without it, one deep
// struct can return megabytes into the model's context.
var loadConfig = map[string]interface{}{
	"followPointers":     true,
	"maxVariableRecurse": 2,
	"maxStringLen":       512,
	"maxArrayValues":     32,
	"maxStructFields":    -1,
}

// Locals returns the local variables in scope at the stop.
func (s *Session) Locals(goroutineID int) ([]Variable, error) {
	var out struct {
		Variables []struct {
			Name, Value, Type string
		}
	}
	in := map[string]interface{}{
		"Scope": map[string]interface{}{"goroutineID": goroutineID, "frame": 0},
		"Cfg":   loadConfig,
	}
	if err := s.call("RPCServer.ListLocalVars", in, &out); err != nil {
		return nil, err
	}
	vars := make([]Variable, 0, len(out.Variables))
	for _, v := range out.Variables {
		vars = append(vars, Variable{Name: v.Name, Value: v.Value, Type: v.Type})
	}
	return vars, nil
}

// Eval evaluates an expression in the stopped frame — the thing a print
// statement would have told you, without editing the file.
func (s *Session) Eval(goroutineID int, expr string) (Variable, error) {
	var out struct {
		Variable struct {
			Name, Value, Type string
		}
	}
	in := map[string]interface{}{
		"Scope": map[string]interface{}{"goroutineID": goroutineID, "frame": 0},
		"Expr":  expr,
		"Cfg":   loadConfig,
	}
	if err := s.call("RPCServer.Eval", in, &out); err != nil {
		return Variable{}, err
	}
	return Variable{Name: expr, Value: out.Variable.Value, Type: out.Variable.Type}, nil
}

// Stack returns the call stack at the stop.
func (s *Session) Stack(goroutineID, depth int) ([]Frame, error) {
	if depth <= 0 {
		depth = 20
	}
	var out struct {
		Locations []struct {
			File     string `json:"file"`
			Line     int    `json:"line"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		}
	}
	in := map[string]interface{}{"Id": goroutineID, "Depth": depth}
	if err := s.call("RPCServer.Stacktrace", in, &out); err != nil {
		return nil, err
	}
	frames := make([]Frame, 0, len(out.Locations))
	for _, l := range out.Locations {
		f := Frame{File: l.File, Line: l.Line}
		if l.Function != nil {
			f.Function = l.Function.Name
		}
		frames = append(frames, f)
	}
	return frames, nil
}

// Close detaches, kills the debugged process, and reaps delve.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	var out struct{}
	// Best-effort: if delve already died, killing the process below is enough.
	_ = s.client.Call("RPCServer.Detach", map[string]interface{}{"Kill": true}, &out)
	_ = s.client.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	return nil
}

// call issues one RPC, refusing once the session is closed so a late call
// reports the real reason instead of a connection error.
func (s *Session) call(method string, in, out interface{}) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("debug session is closed")
	}
	if err := s.client.Call(method, in, out); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimPrefix(method, "RPCServer."), err)
	}
	return nil
}
