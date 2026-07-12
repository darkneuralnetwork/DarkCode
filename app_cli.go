package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/darkcode/cli"
	"github.com/darkcode/permission"
	"github.com/darkcode/ui"
)

func (a *AppRunner) RunCLI() {
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
