package embedded

// manager.go — ProcessManager manages the lifecycle of a local llama-server
// process. llama-server (from llama.cpp) exposes an OpenAI-compatible HTTP
// API, so once it's running we can talk to it like any other OpenAI endpoint.
//
// This is the "Local-First Execution" backbone (spec §3): it lets DarkCode
// run entirely offline by spawning a local model server and proxying chat
// completions through it. No CGo required — just the llama-server binary on
// PATH or in binaryDir.
//
// Previously this was an incomplete stub: it assumed the binary existed,
// always used port 8080, didn't wait for readiness, and had no status/health
// check. It now:
//   - Discovers the binary (PATH lookup + binaryDir override)
//   - Allocates a free port (no hard-coded 8080)
//   - Waits for the /health endpoint to respond before returning
//   - Exposes Status() and the base URL for the OpenAI client
//   - Shuts down gracefully (SIGTERM → SIGKILL fallback)

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/observability"
)

type ProcessState int

const (
	StateStopped ProcessState = iota
	StateStarting
	StateRunning
	StateStopping
	StateFailed
)

func (s ProcessState) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// ProcessManager manages the lifecycle of the local llama-server binary.
type ProcessManager struct {
	mu        sync.Mutex
	serverCmd *exec.Cmd
	binaryDir string
	port      int
	state     ProcessState
	modelPath string
	startedAt time.Time
	// waitDone is closed by a single goroutine that calls serverCmd.Wait(),
	// so both waitForReady (early-exit fast-fail) and Stop (graceful reap)
	// observe process exit without ever calling Wait() twice concurrently
	// (which would race on exec.Cmd). nil when no process is running.
	waitDone chan struct{}
	// stderr captures llama-server's stdout+stderr so a failed/early-exit
	// start surfaces the real error (e.g. "failed to open GGUF file …")
	// instead of the generic "did not become healthy within 60s". It is
	// concurrency-safe (written by exec pipe goroutines, read by
	// waitForReady) and bounded to the last stderrTailCap bytes.
	stderr  *tailBuffer
	loraMap map[string]int // Maps LoRA name to ID (0-based) for the /lora-adapters endpoint

	// onCrash is invoked (in its own goroutine, never while p.mu is held) when
	// the background health monitor decides the process has died or stopped
	// responding after a successful Start(). nil = no restart policy installed.
	onCrash func()
	// healthStop is closed by Stop() to tell the current health-loop
	// goroutine to exit cleanly instead of treating a deliberate stop as a
	// crash. nil when no health loop is running.
	healthStop chan struct{}
}

const stderrTailCap = 4096

// Background health-monitor tuning. A failure is only reported after
// healthCheckMaxFails consecutive misses (~healthCheckInterval *
// healthCheckMaxFails to detect), so a single slow response under load isn't
// mistaken for a crash.
const (
	healthCheckInterval = 20 * time.Second
	healthCheckMaxFails = 3
)

// NewProcessManager creates a manager. binaryDir is an optional override
// directory where llama-server lives (empty → use PATH lookup).
func NewProcessManager(binaryDir string) *ProcessManager {
	return &ProcessManager{
		binaryDir: binaryDir,
		state:     StateStopped,
		stderr:    newTailBuffer(stderrTailCap),
	}
}

// SetOnCrash installs a callback invoked when the background health monitor
// detects the process has died or become unresponsive after a successful
// Start(). Call this once at construction (see Default() in embedded_stub.go)
// — it is not itself concurrency-guarded against being changed mid-flight.
func (p *ProcessManager) SetOnCrash(fn func()) {
	p.mu.Lock()
	p.onCrash = fn
	p.mu.Unlock()
}

// SetBinaryDir updates the binary discovery directory. Used by the singleton
// provider so a later Configure() call (e.g. from the wiring layer with the
// downloaded-bin dir) takes effect without recreating the manager. Empty
// values are ignored so a read-only caller cannot clobber a known dir.
func (p *ProcessManager) SetBinaryDir(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if dir != "" {
		p.binaryDir = dir
	}
}

// findBinary resolves the llama-server executable path.
func (p *ProcessManager) findBinary() (string, error) {
	exeName := "llama-server"
	if runtime.GOOS == "windows" {
		exeName = "llama-server.exe"
	}
	// 1. Explicit override.
	if p.binaryDir != "" {
		candidate := filepath.Join(p.binaryDir, exeName)
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	// 2. PATH lookup.
	if path, err := exec.LookPath(exeName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("llama-server binary not found (looked in PATH%s)", pathSuffix(p.binaryDir))
}

func pathSuffix(dir string) string {
	if dir == "" {
		return ""
	}
	return " and " + dir
}

// freePort asks the OS for an available TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// LaunchOpts configures how the llama-server process is started. Zero
// values preserve the previous (CPU-only, 16384 ctx, 4 slots) behavior so
// existing single-model setups are unchanged.
type LaunchOpts struct {
	// NGPULayers is the number of layers to offload to the GPU (-ngl). 0 = CPU
	// only (the safe default). Set >0 only when a supported GPU is detected.
	NGPULayers int
	// ContextSize overrides the -c context window (default 16384).
	ContextSize int
	// Parallel overrides the -np decode slots (default 4).
	Parallel int
	// LoRADir is the directory to scan for .gguf LoRA adapters to pre-load.
	LoRADir string
	// DisableKVCacheQuant opts out of the default --cache-type-k/-v q8_0
	// flags (e.g. for a llama-server build that doesn't support them).
	DisableKVCacheQuant bool
}

// Start spawns the llama-server process and waits for it to become healthy.
// opts tunes launch parameters (GPU offload, context size, parallel slots);
// a zero-value opts reproduces the legacy CPU-only launch.
func (p *ProcessManager) Start(ctx context.Context, modelPath string, opts LaunchOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == StateRunning || p.state == StateStarting {
		return fmt.Errorf("embedded server already %s", p.state)
	}

	binPath, err := p.findBinary()
	if err != nil {
		p.state = StateFailed
		return err
	}

	port, err := freePort()
	if err != nil {
		p.state = StateFailed
		return fmt.Errorf("failed to allocate port: %w", err)
	}
	p.port = port
	p.modelPath = modelPath
	p.state = StateStarting

	ctxSz := opts.ContextSize
	if ctxSz == 0 {
		ctxSz = 16384
	}
	parallel := opts.Parallel
	if parallel == 0 {
		parallel = 4
	}

	args := []string{
		"-m", modelPath,
		"--port", fmt.Sprintf("%d", port),
		"-c", fmt.Sprintf("%d", ctxSz),
		"-np", fmt.Sprintf("%d", parallel),
		"--host", "127.0.0.1",
		"--embedding",
	}
	if opts.NGPULayers > 0 {
		args = append(args, "-ngl", fmt.Sprintf("%d", opts.NGPULayers))
	}
	// KV-cache quantization: q8_0 roughly halves KV-cache RAM at near-zero
	// quality loss vs the f16 default, which is what actually lets the large
	// context windows above (32K/128K) fit on lower-RAM systems. Opt-out via
	// LaunchOpts.DisableKVCacheQuant for the rare case a build doesn't
	// support these flags.
	if !opts.DisableKVCacheQuant {
		args = append(args, "--cache-type-k", "q8_0", "--cache-type-v", "q8_0")
	}

	p.loraMap = make(map[string]int)
	loraDir := opts.LoRADir
	if loraDir == "" {
		loraDir = defaultLoRADir()
	}
	// Migration fallback: the old download_loras.sh wrote adapters to a
	// CWD-relative ./loras, but the manager scans the system-wide dir. If the
	// system-wide dir has none and a legacy ./loras does, use it and tell the
	// user to move them so discovery stops depending on the launch directory.
	if matches, _ := filepath.Glob(filepath.Join(loraDir, "*.gguf")); len(matches) == 0 {
		if legacy, _ := filepath.Glob(filepath.Join("loras", "*.gguf")); len(legacy) > 0 {
			observability.Log().Warn("using LoRA adapters from ./loras — move them to the system-wide dir so discovery no longer depends on the launch directory", map[string]interface{}{"legacy_dir": "./loras", "system_dir": loraDir})
			loraDir = "loras"
		}
	}
	if loraFiles, err := filepath.Glob(filepath.Join(loraDir, "*.gguf")); err == nil && len(loraFiles) > 0 {
		sort.Strings(loraFiles)
		// Pre-load all LoRAs with scale 0 (so they don't apply immediately).
		// llama-server expects a comma-separated list for repeated --lora values.
		args = append(args, "--lora-init-without-apply", "--lora", strings.Join(loraFiles, ","))
		for i, f := range loraFiles {
			base := filepath.Base(f)
			name := strings.TrimSuffix(base, ".gguf")
			p.loraMap[name] = i
		}
	}

	// Speculative decoding draft model support
	draftPath := filepath.Join(filepath.Dir(modelPath), "draft.gguf")
	if _, err := os.Stat(draftPath); err == nil {
		args = append(args, "--draft", draftPath)
	}

	p.serverCmd = exec.CommandContext(ctx, binPath, args...)
	// Capture stderr (and stdout) into a bounded buffer so a failed start
	// reports the real cause. Previously both were nil, so the only signal on
	// a bad model path was a 60s "did not become healthy" timeout with no
	// detail. The buffer is trimmed to its tail before being surfaced.
	p.stderr.Reset()
	p.serverCmd.Stdout = p.stderr
	p.serverCmd.Stderr = p.stderr
	p.serverCmd.Env = os.Environ()
	if runtime.GOOS == "linux" {
		binDir := filepath.Dir(binPath)
		p.serverCmd.Env = append(p.serverCmd.Env, "LD_LIBRARY_PATH="+binDir+":"+os.Getenv("LD_LIBRARY_PATH"))
	} else if runtime.GOOS == "darwin" {
		binDir := filepath.Dir(binPath)
		p.serverCmd.Env = append(p.serverCmd.Env, "DYLD_LIBRARY_PATH="+binDir+":"+os.Getenv("DYLD_LIBRARY_PATH"))
	}

	if err := p.serverCmd.Start(); err != nil {
		p.state = StateFailed
		return fmt.Errorf("failed to start embedded runtime: %w", err)
	}

	// Single reaper goroutine: closes waitDone when the process exits. Both
	// waitForReady (early-exit fast-fail) and Stop (graceful reap) select on
	// this channel instead of calling Wait() themselves, so Wait() is never
	// invoked twice concurrently (which would race on exec.Cmd).
	p.waitDone = make(chan struct{})
	go func() {
		_ = p.serverCmd.Wait()
		close(p.waitDone)
	}()

	// Wait for the /health endpoint to respond (up to 60s for big models).
	// waitForReady also listens on waitDone so an early process death is
	// detected in milliseconds rather than burning the full 60s timeout (the
	// old code checked ProcessState.Exited(), which is nil until Wait()
	// returns — so it never fired).
	if err := p.waitForReady(ctx, 60*time.Second); err != nil {
		p.state = StateFailed
		if p.serverCmd.Process != nil {
			_ = p.serverCmd.Process.Kill()
		}
		<-p.waitDone // let the reaper goroutine finish so the process is reaped
		p.waitDone = nil
		return err
	}

	p.state = StateRunning
	p.startedAt = time.Now()

	// Background health monitor: waitForReady above only proves the process
	// was healthy at startup. Without this, a llama-server that later
	// crashes or wedges is only ever discovered when a real request fails —
	// silently, mid-task. healthStop/waitDone are captured now (under p.mu)
	// so the goroutine tracks this specific process instance, not whatever
	// Start()/Stop() does next.
	healthStop := make(chan struct{})
	p.healthStop = healthStop
	go p.healthLoop(p.port, p.waitDone, healthStop)

	return nil
}

// healthLoop periodically polls /health after a successful Start() and
// reports a probable crash (via onCrash) if it stops responding for
// healthCheckMaxFails consecutive checks, or if the process exits without a
// deliberate Stop() (detected via waitDone closing). It exits cleanly,
// without reporting anything, when stop is closed (Stop() was called) or the
// state has moved on for any other reason (e.g. a manual model swap).
func (p *ProcessManager) healthLoop(port int, waitDone, stop chan struct{}) {
	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	fails := 0
	for {
		select {
		case <-stop:
			return
		case <-waitDone:
			p.reportCrash()
			return
		case <-ticker.C:
			p.mu.Lock()
			state := p.state
			p.mu.Unlock()
			if state != StateRunning {
				return
			}
			resp, err := client.Get(url)
			healthy := err == nil && resp != nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				_ = resp.Body.Close()
			}
			if healthy {
				fails = 0
				continue
			}
			fails++
			if fails >= healthCheckMaxFails {
				p.reportCrash()
				return
			}
		}
	}
}

// reportCrash marks the process failed and invokes the installed onCrash
// callback (if any), asynchronously and outside the lock so the callback is
// free to call back into the ProcessManager (e.g. to restart it). It's a
// no-op if the state has already moved on (deliberate Stop()/swap raced with
// the health check), which is what keeps healthLoop's Stop()-vs-crash race
// safe regardless of which fires first.
func (p *ProcessManager) reportCrash() {
	p.mu.Lock()
	if p.state != StateRunning {
		p.mu.Unlock()
		return
	}
	p.state = StateFailed
	onCrash := p.onCrash
	p.mu.Unlock()
	if onCrash != nil {
		go onCrash()
	}
}

// waitForReady polls the /health endpoint until it responds or timeout. It
// also listens on the `waitDone` channel (closed by the single Wait() reaper
// goroutine started in Start) so an early process death is detected in
// milliseconds rather than burning the full timeout. Any captured stderr tail
// is included in the returned error.
func (p *ProcessManager) waitForReady(ctx context.Context, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", p.port)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-p.waitDone:
			return fmt.Errorf("llama-server exited prematurely: %s", p.stderrTail())
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("llama-server did not become healthy within %s: %s", timeout, p.stderrTail())
		}
	}
}

// SetLoRAScale dynamically adjusts the scale of a pre-loaded LoRA adapter via the /lora-adapters endpoint.
func (p *ProcessManager) SetLoRAScale(ctx context.Context, id int, scale float32) error {
	p.mu.Lock()
	port := p.port
	p.mu.Unlock()

	if port == 0 {
		return fmt.Errorf("server not running")
	}

	payload := fmt.Sprintf(`[{"id": %d, "scale": %.2f}]`, id, scale)
	url := fmt.Sprintf("http://127.0.0.1:%d/lora-adapters", port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to set LoRA scale: HTTP %d", resp.StatusCode)
	}
	return nil
}

// GetLoRAID returns the ID (index) for a given LoRA name, if it was loaded.
func (p *ProcessManager) GetLoRAID(name string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.loraMap[name]
	return id, ok
}

// stderrTail returns up to the last stderrTailCap bytes of captured
// llama-server output, trimmed for inclusion in error messages. Safe to
// call concurrently with the exec pipe writers.
func (p *ProcessManager) stderrTail() string {
	return strings.TrimSpace(p.stderr.Tail())
}

// tailBuffer is a concurrency-safe, capacity-bounded byte buffer that keeps
// only the trailing `cap` bytes of everything written to it. It is used to
// capture llama-server output without growing unbounded over a long-running
// server's lifetime while always retaining the most recent (most relevant)
// output for error reporting.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func newTailBuffer(capBytes int) *tailBuffer {
	return &tailBuffer{cap: capBytes}
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len()+len(b) > t.cap {
		over := (t.buf.Len() + len(b)) - t.cap
		if over < t.buf.Len() {
			t.buf.Next(over)
		} else {
			t.buf.Reset()
			if len(b) > t.cap {
				b = b[len(b)-t.cap:]
			}
		}
	}
	return t.buf.Write(b)
}

// Tail returns a copy of the retained trailing bytes.
func (t *tailBuffer) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.buf.String()
	if len(s) > t.cap {
		s = s[len(s)-t.cap:]
	}
	return s
}

// Reset empties the buffer.
func (t *tailBuffer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Reset()
}

// Stop cleanly terminates the llama-server process.
func (p *ProcessManager) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Signal state away from StateRunning and stop the health loop BEFORE
	// touching the process, so a concurrent healthLoop tick either sees the
	// new state (and exits quietly) or is told directly via healthStop —
	// either way it can't misreport this deliberate stop as a crash.
	if p.healthStop != nil {
		close(p.healthStop)
		p.healthStop = nil
	}
	if p.serverCmd == nil || p.serverCmd.Process == nil {
		p.state = StateStopped
		p.waitDone = nil
		return
	}
	p.state = StateStopping
	// Try graceful termination first (SIGTERM on Unix).
	_ = p.serverCmd.Process.Signal(termSignal)
	// Wait for the single reaper goroutine (started in Start) to reap the
	// process; fall back to SIGKILL if it doesn't exit within 10s. We never
	// call Wait() here directly — that would race with the reaper goroutine.
	select {
	case <-p.waitDone:
	case <-time.After(10 * time.Second):
		_ = p.serverCmd.Process.Kill()
		<-p.waitDone
	}
	p.state = StateStopped
	p.serverCmd = nil
	p.waitDone = nil
}

// Status returns the current state of the embedded server.
func (p *ProcessManager) Status() ProcessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProcessStatus{
		State:     p.state,
		Port:      p.port,
		ModelPath: p.modelPath,
		Uptime:    uptime(p.startedAt, p.state),
		BaseURL:   p.baseURL(),
	}
}

// baseURL returns the OpenAI-compatible API base URL (empty if not running).
func (p *ProcessManager) baseURL() string {
	if p.port == 0 || p.state != StateRunning {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", p.port)
}

// BaseURL is the thread-safe accessor for the API base URL.
func (p *ProcessManager) BaseURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baseURL()
}

// ProcessStatus is a snapshot of the embedded server's state.
type ProcessStatus struct {
	State     ProcessState
	Port      int
	ModelPath string
	Uptime    time.Duration
	BaseURL   string
}

func uptime(started time.Time, state ProcessState) time.Duration {
	if state != StateRunning || started.IsZero() {
		return 0
	}
	return time.Since(started)
}

// defaultLoRADir returns the system-wide LoRA directory
// (~/.darkcode/models/loras) used when LaunchOpts.LoRADir isn't explicitly
// set, falling back to a CWD-relative path only if the home directory can't
// be resolved. Consolidates with the app layer's models directory
// (app_wireup.go's defaultDarkcodeDir("models")) instead of the previous
// always-CWD-relative "./loras" default.
func defaultLoRADir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".darkcode", "models", "loras")
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".darkcode", "models", "loras")
}
