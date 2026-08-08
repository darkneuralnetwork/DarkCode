package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/darkcode/checkpoint"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/hooks"
	"github.com/darkcode/intelligence"
	"github.com/darkcode/memory"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
	"github.com/darkcode/recall"
	"github.com/darkcode/router"
	"github.com/darkcode/security"
	"github.com/darkcode/server"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
	"github.com/darkcode/uiport"
)

type AppRunner struct {
	Cfg       *config.Config
	Registry  *tools.Registry
	SourceMgr *tools.SourceManager
	MemSystem *memory.System
	// Recall is the single gateway for remembering a fact. Placement is its
	// decision, not each caller's — see the recall package.
	Recall       *recall.Manager
	ProjectStore *project.Store
	Emitter      *ui.EventEmitter
	Router       *router.Router
	Compressor   *compression.Compressor
	// createClient builds an LLM client from a model config (handling the
	// embedded provider). It is the wiring layer's factory, persisted here so
	// the kernel's live model-reload can reuse it without importing llm.
	createClient func(config.ModelConfig) core.LLMClient
	Kernel       *orchestrator.Kernel
	// Port is the single way a surface reaches the kernel. Every surface
	// goes through it so none can decide for itself whether a request
	// carries a workspace — see uiport for what that omission cost.
	Port         *uiport.Manager
	Recorder     *tools.ChangeRecorder
	Checkpoints  *checkpoint.Manager
	LSP          *intelligence.LSPClient
	HealthDaemon *memory.HealthDaemon
	// Policy is the restriction set applied on top of Cfg. Empty when no
	// policy file is present, which is the common case.
	Policy   config.Policy
	Patterns *memory.PatternLibrary
	Server   *server.Server

	PluginLoader *plugin.Loader
	PluginHost   *plugin.Host
	// ExtCommands are the slash commands loaded bundles offer. The console
	// consults them before reporting an unknown command.
	ExtCommands []tools.ExtensionCommand
	// Hooks runs the user's configured commands at the lifecycle points. nil
	// when none are configured, which is a valid no-op everywhere.
	Hooks   *hooks.Manager
	Sandbox *security.Sandbox

	StatusOnly bool
	PortFlag   string
	BindAddr   string
	GuiFlag    bool

	serverStarted       bool
	globalActiveProject string
	resumedFromGUI      bool
	mode                string
	localLLMPromptShown bool

	shutdownOnce sync.Once
}

func NewAppRunner(cfg *config.Config, statusOnly bool, portFlag string, guiFlag bool, bindAddr string) *AppRunner {
	return &AppRunner{
		Cfg:        cfg,
		StatusOnly: statusOnly,
		PortFlag:   portFlag,
		GuiFlag:    guiFlag,
		BindAddr:   bindAddr,
	}
}

func (a *AppRunner) Execute() {
	a.installSignalHandler()
	if a.StatusOnly {
		fmt.Println(a.Kernel.Status())
		fmt.Println("\nRegistered Tools:")
		for _, entry := range a.Registry.List() {
			fmt.Printf("  - %-15s [%s]\n", entry.Name, entry.Category)
		}
		fmt.Println("\n" + a.MemSystem.Summary())
		os.Exit(0)
	}

	a.mode = "cli"
	if a.GuiFlag {
		a.mode = "gui"
	}

	for {
		if a.mode == "cli" {
			a.RunCLI()
		} else if a.mode == "gui" {
			a.RunGUI()
		} else {
			break
		}
	}
}

// Shutdown cleans up all resources: plugin processes, embedded models, etc.
// Should be called on application exit (e.g. via defer or signal handler).
func (a *AppRunner) Shutdown() {
	if a.PluginHost != nil {
		a.PluginHost.Shutdown()
	}
}

// installSignalHandler flushes state on SIGINT/SIGTERM before exit. Without it,
// killing the persistent GUI (SIGTERM on rebuild/restart) dropped the last
// debounce window of memory/knowledge-graph writes. readline reads an
// interactive Ctrl-C as a raw byte, not a signal, so this never interferes with
// the CLI's line-cancel — it only fires on a real delivered signal.
func (a *AppRunner) installSignalHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		a.gracefulShutdown()
		os.Exit(0)
	}()
}

// gracefulShutdown persists memory and tears down resources. Idempotent so the
// signal path and any normal-exit path can both call it safely.
func (a *AppRunner) gracefulShutdown() {
	a.shutdownOnce.Do(func() {
		if a.Server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = a.Server.Shutdown(ctx)
		}
		if a.MemSystem != nil {
			a.MemSystem.Shutdown() // flushes episodic/semantic/procedural/KG writers
		}
		if a.LSP != nil {
			a.LSP.Shutdown() // stop any language server processes we started
		}
		a.Shutdown()
	})
}
