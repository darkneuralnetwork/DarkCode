package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/darkcode/permission"
	"github.com/darkcode/ui"
)

func (a *AppRunner) RunGUI() {
	if ma := a.Kernel.ModeApprover(); ma != nil {
		ma.SetMode(permission.ModeGUI)
	}
	a.Emitter.SetMode(ui.OutputSSE)
	a.Emitter.EmitSyncGUI()
	if a.PortFlag == "" {
		a.PortFlag = "12345"
	}
	if !a.serverStarted {
		if a.BindAddr == "" {
			a.BindAddr = "127.0.0.1:" + a.PortFlag
		}
		fmt.Println("\033[1mStarting Web UI...\033[0m")
		fmt.Printf("  GUI URL: http://localhost:%s\n", a.PortFlag)
		errCh := make(chan error, 1)
		go func() {
			if err := a.Server.Start(a.BindAddr); err != nil {
				errCh <- err
			}
		}()
		
		// Wait briefly to see if server crashes immediately (e.g., port in use)
		select {
		case err := <-errCh:
			fmt.Fprintf(os.Stderr, "Server already running on port %s (or bind failed): %v\n", a.PortFlag, err)
			// Don't return, we can still open the browser pointing to the existing server!
			a.serverStarted = false
		case <-time.After(100 * time.Millisecond):
			a.serverStarted = true
		}
	}
	a.Server.SetActiveProject(a.globalActiveProject)
	a.Server.SetGUIActive(true)

	select {
	case <-a.Server.SwitchToCLI:
	default:
	}

	url := "http://localhost:" + a.PortFlag
	openBrowser(url)

	if !a.serverStarted {
		fmt.Println("\n\033[38;5;214mDarkCode GUI is being served by a background instance.\033[0m")
		fmt.Println("Browser opened. Press [Enter] here to resume this CLI.")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
	} else {
		a.globalActiveProject = <-a.Server.SwitchToCLI
	}

	a.mode = "cli"
	a.resumedFromGUI = true
	fmt.Println("\n\033[38;5;39mSwitching back to CLI mode...\033[0m")
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		// Try multiple common browser launchers on Linux
		cmds := []string{"xdg-open", "x-www-browser", "sensible-browser", "gnome-open", "kde-open"}
		for _, cmd := range cmds {
			err = exec.Command(cmd, url).Start()
			if err == nil {
				break
			}
		}
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[38;5;214mCould not open browser automatically.\033[0m\n")
		fmt.Fprintf(os.Stderr, "Please open this URL manually: \033[1m%s\033[0m\n\n", url)
	}
}
