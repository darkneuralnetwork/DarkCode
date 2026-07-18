package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/darkcode/cli"
	"github.com/darkcode/core"
	"github.com/darkcode/permission"
	"github.com/darkcode/ui"
)

func (a *AppRunner) RunCLI() {
	a.maybePromptEnableLocalLLM()
	a.Emitter.SetMode(ui.OutputNone)
	a.Server.SetGUIActive(false)
	if ma := a.Kernel.ModeApprover(); ma != nil {
		ma.SetMode(permission.ModeCLI)
	}
	console := cli.NewConsole(a.Cfg, a.Kernel, a.MemSystem, a.Registry, a.Emitter, a.Recorder, a.SourceMgr, a.ProjectStore, a.globalActiveProject)
	console.SetResumed(a.resumedFromGUI)
	a.resumedFromGUI = false
	err := console.Run()
	if errors.Is(err, cli.ErrSwitchToGUI) {
		a.globalActiveProject = console.ActiveProject()
		a.mode = "gui"
		fmt.Println("\n\033[38;5;39mSwitching to GUI mode...\033[0m")
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	} else {
		a.mode = "exit"
	}
}

// maybePromptEnableLocalLLM asks once per process whether to enable the
// local embedded model when the router ended up with zero configured
// models — e.g. the user skipped the first-run setup wizard entirely, or
// every configured model failed to register. Guarded by
// localLLMPromptShown so switching CLI↔GUI mid-session (Execute's mode
// loop) never re-prompts. On yes, saves the choice AND loads the model
// immediately via the same path normal startup uses
// (AppRunner.loadLocalLLM), so it's usable without a restart.
func (a *AppRunner) maybePromptEnableLocalLLM() {
	if a.localLLMPromptShown || a.Router == nil || a.Router.ModelCount() > 0 {
		return
	}
	a.localLLMPromptShown = true

	fmt.Println("\nNo LLM is currently available.")
	fmt.Print("Enable the local embedded model (llama-server)? [Y/n] > ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line != "" && line != "y" && line != "yes" {
		return
	}

	a.Cfg.EnableLocalLLM = true
	if err := a.Cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save config: %v\n", err)
	}
	if a.loadLocalLLM(core.ParseRoutingMode(a.Cfg.RoutingMode)) != nil {
		fmt.Println("Local model loaded.")
	} else {
		fmt.Println("Could not load a local model — check the logs for details. Retry later with '/local on'.")
	}
}
