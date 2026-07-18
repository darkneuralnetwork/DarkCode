package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/chzyer/readline"
)

// RunInteractiveSetup launches a simple command-line wizard to collect
// the user's API key when starting up for the first time.
func RunInteractiveSetup(cfg *Config) error {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	fmt.Println("\033[38;5;208m\033[1mWelcome to DarkCode!\033[0m")
	fmt.Println("It looks like you don't have an API key configured yet.")
	fmt.Println("Let's get you set up.")
	fmt.Println("Select your provider:")

	providersList := Providers()
	for i, p := range providersList {
		rec := ""
		if p.ID == "openrouter" {
			rec = " (Recommended)"
		}
		fmt.Printf("  %d) %s%s\n", i+1, p.Name, rec)
	}
	skipIdx := len(providersList) + 1
	fmt.Printf("  %d) Skip setup\n", skipIdx)

	var provider, baseURL, model string
	for {
		rl.SetPrompt(fmt.Sprintf("Provider (1-%d) > ", skipIdx))
		line, err := rl.Readline()
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)

		idx, err := strconv.Atoi(line)
		if err != nil || idx < 1 || idx > skipIdx {
			fmt.Printf("Invalid choice. Please enter a number 1-%d.\n", skipIdx)
			continue
		}

		if idx == skipIdx {
			fmt.Println("Skipping setup. You can configure this later via the GUI or '/models add'.")
			cfg.Provider = ""
			cfg.BaseURL = ""
			cfg.Model = ""
			cfg.APIKey = ""
			cfg.EnableLocalLLM = askEnableLocalLLM(rl)
			cfg.Save()
			return nil
		}

		selected := providersList[idx-1]
		provider = selected.ID
		baseURL = selected.BaseURL
		if len(selected.Models) > 0 {
			model = selected.Models[0].ID
		}
		break
	}

	for {
		rl.SetPrompt("Enter your API key > ")
		line, err := rl.Readline()
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			fmt.Println("API key cannot be empty. Press Ctrl+C to cancel.")
			continue
		}
		cfg.APIKey = line
		cfg.Provider = provider
		cfg.BaseURL = baseURL
		cfg.Model = model
		break
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save config: %v\n", err)
	} else {
		fmt.Printf("\033[32mSuccess!\033[0m Configuration saved to %s\n\n", ConfigPath())
	}
	return nil
}

// askEnableLocalLLM prompts whether to enable the local embedded model
// (llama-server) when the user has no cloud provider configured, so a
// skipped setup doesn't silently leave them with zero usable models. Defaults
// to yes on blank input — a user who skipped cloud setup almost certainly
// wants *some* working model, and local is the only option left.
func askEnableLocalLLM(rl *readline.Instance) bool {
	fmt.Println("No cloud model configured.")
	rl.SetPrompt("Enable the local embedded model (llama-server)? [Y/n] > ")
	line, err := rl.Readline()
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}
