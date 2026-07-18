package main

import (
	"context"
	"fmt"
	"os"

	"github.com/darkcode/cli"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/memory"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
	"github.com/darkcode/router"
	"github.com/darkcode/security"
	"github.com/darkcode/server"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

type AppRunner struct {
	Cfg          *config.Config
	Registry     *tools.Registry
	SourceMgr    *tools.SourceManager
	MemSystem    *memory.System
	ProjectStore *project.Store
	Emitter      *ui.EventEmitter
	Router       *router.Router
	Compressor   *compression.Compressor
	Kernel       *orchestrator.Kernel
	Recorder     *tools.ChangeRecorder
	Server       *server.Server

	PluginLoader *plugin.Loader
	PluginHost   *plugin.Host
	Sandbox      *security.Sandbox

	StatusOnly bool
	Query      string
	PortFlag   string
	BindAddr   string
	GuiFlag    bool

	serverStarted       bool
	globalActiveProject string
	resumedFromGUI      bool
	mode                string
	localLLMPromptShown bool
}

func NewAppRunner(cfg *config.Config, query string, statusOnly bool, portFlag string, guiFlag bool, bindAddr string) *AppRunner {
	return &AppRunner{
		Cfg:        cfg,
		Query:      query,
		StatusOnly: statusOnly,
		PortFlag:   portFlag,
		GuiFlag:    guiFlag,
		BindAddr:   bindAddr,
	}
}

func (a *AppRunner) Execute() {
	if a.StatusOnly {
		fmt.Println(a.Kernel.Status())
		fmt.Println("\nRegistered Tools:")
		for _, entry := range a.Registry.List() {
			fmt.Printf("  - %-15s [%s]\n", entry.Name, entry.Category)
		}
		fmt.Println("\n" + a.MemSystem.Summary())
		os.Exit(0)
	}

	if a.Query != "" {
		ctx := context.Background()
		a.Kernel.Gate().SetApprover(permission.AutoApprover())
		if a.Cfg.UIMode {
			a.Emitter.EmitTaskUpdate("kernel", "observe", a.Query)
		}
		result, err := a.Kernel.Execute(ctx, a.Query)
		if err != nil {
			a.MemSystem.Shutdown()
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if changes := a.Recorder.All(); len(changes) > 0 {
			cli.PrintChanges(os.Stderr, changes)
		}
		if !a.Cfg.UIMode {
			fmt.Println(result)
		}
		a.MemSystem.Shutdown()
		return
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
