package tools

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/darkcode/infra/security"
)

func TestTerminalStrictRefusesWithoutBackend(t *testing.T) {
	// strict mode + no backend must fail closed instead of running unconfined.
	sb := &security.Sandbox{Mode: security.ModeStrict, Backend: security.BackendNone}
	term := NewTerminalTool(sb)

	res := term.Execute(context.Background(), map[string]interface{}{"command": "echo hi"})
	if res.Success {
		t.Fatal("strict sandbox with no backend must refuse to run the command")
	}
	if !strings.Contains(res.Error, "strict") {
		t.Errorf("refusal should explain the strict-mode cause, got %q", res.Error)
	}
}

func TestTerminalRunsWhenNoSandbox(t *testing.T) {
	// No sandbox injected => command runs (unconfined), preserving old behavior.
	term := NewTerminalTool(nil)
	res := term.Execute(context.Background(), map[string]interface{}{"command": "echo darkcode-ok"})
	if !res.Success || !strings.Contains(res.Output, "darkcode-ok") {
		t.Fatalf("plain command should run: success=%v out=%q err=%q", res.Success, res.Output, res.Error)
	}
}

// TestTerminalKillsProcessGroupOnTimeout exercises mission Scenario 6/10
// (timeout + interrupt mid-execution) end to end rather than just reading
// cmd.Cancel/killProcessGroup: a command that backgrounds a child process
// must not leave that child running after the parent is killed by the
// timeout. Killing only the direct process (not the process group) would
// leak the backgrounded sleep.
func TestTerminalKillsProcessGroupOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill via negative PID is unix-specific (see terminal_unix.go / terminal_windows.go)")
	}
	pidFile, err := os.CreateTemp("", "darkcode-child-pid-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	pidPath := pidFile.Name()
	pidFile.Close()
	defer os.Remove(pidPath)

	term := NewTerminalTool(nil)
	cmd := fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidPath)
	res := term.Execute(context.Background(), map[string]interface{}{
		"command": cmd,
		"timeout": float64(1),
	})
	if res.Success {
		t.Fatalf("expected the command to be killed by the 1s timeout, got success: %+v", res)
	}

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil || len(strings.TrimSpace(string(pidBytes))) == 0 {
		t.Fatalf("child pid was never written — the background sleep may not have started: err=%v content=%q", err, pidBytes)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("could not parse child pid %q: %v", pidBytes, err)
	}

	// Give the SIGKILL a moment to be reaped, then confirm the backgrounded
	// child is actually gone, not just the parent shell.
	deadline := time.Now().Add(2 * time.Second)
	var alive bool
	for {
		alive = syscall.Kill(childPID, 0) == nil
		if !alive || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive {
		t.Fatalf("child process %d (backgrounded by the timed-out command) is still running — the process group was not killed", childPID)
	}
}

// TestLimitedBufferCapsWrites guards against unbounded memory growth while a
// command runs: a plain bytes.Buffer (the pre-fix implementation) would keep
// every byte a runaway command writes, in memory, before the timeout ever
// gets a chance to stop it.
func TestLimitedBufferCapsWrites(t *testing.T) {
	lb := &limitedBuffer{max: 10}
	n, err := lb.Write([]byte("0123456789ABCDE")) // 15 bytes into a 10-byte cap
	if err != nil {
		t.Fatalf("Write must never error (exec.Cmd would misreport exit status via io.ErrShortWrite): %v", err)
	}
	if n != 15 {
		t.Fatalf("Write must report the full length accepted, matching bytes.Buffer's contract, got %d", n)
	}
	if lb.buf.Len() != 10 {
		t.Fatalf("buffered bytes must be capped at max=10, got %d (unbounded growth)", lb.buf.Len())
	}
	if lb.dropped != 5 {
		t.Fatalf("dropped must count bytes discarded past the cap, got %d, want 5", lb.dropped)
	}
	if lb.String() != "0123456789" {
		t.Fatalf("kept content must be the first max bytes, got %q", lb.String())
	}
}
