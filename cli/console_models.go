package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/cli/tui"
	"github.com/darkcode/config"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/llm"
	provpkg "github.com/darkcode/provider"
)

// handleModels dispatches /models subcommands.
func (c *Console) handleModels(args []string) {
	if len(args) == 0 {
		c.listModels()
		return
	}
	switch args[0] {
	case "add":
		c.modelAdd(args[1:])
	case "remove", "rm":
		c.modelRemove(args[1:])
	case "primary", "use":
		c.modelPrimary(args[1:])
	case "test", "ping":
		c.modelTest(args[1:])
	case "disable":
		c.modelDisable(args[1:])
	case "enable":
		c.modelEnable(args[1:])
	default:
		fmt.Printf("%s usage: /models [add|remove|primary|test|disable|enable]\n", paint(cRed, "✗"))
	}
}

// modelDisable temporarily takes a model out of routing/consensus selection
// (local-first upgrade §6c). Usage: /models disable <name> [duration]
// (duration is a Go duration string like "1h", "30m"; default "1h" if
// omitted).
func (c *Console) modelDisable(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models disable <name> [duration] (e.g. /models disable gpt-4 1h)\n", paint(cRed, "✗"))
		return
	}
	name := args[0]
	durStr := "1h"
	if len(args) > 1 {
		durStr = args[1]
	}
	dur, err := time.ParseDuration(durStr)
	if err != nil {
		fmt.Printf("%s invalid duration %q: %v (try e.g. \"1h\", \"30m\")\n", paint(cRed, "✗"), durStr, err)
		return
	}
	if c.kernel == nil {
		fmt.Printf("%s no kernel available — cannot disable models\n", paint(cRed, "✗"))
		return
	}
	c.kernel.DisableModel(name, time.Now().Add(dur))
	fmt.Printf("%s %s disabled for %s\n", paint(cGreen, "✓"), paint(cWhite, name), dur)
}

// modelEnable reverses a temporary disable early. Usage: /models enable <name>
func (c *Console) modelEnable(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models enable <name>\n", paint(cRed, "✗"))
		return
	}
	name := args[0]
	if c.kernel == nil {
		fmt.Printf("%s no kernel available — cannot enable models\n", paint(cRed, "✗"))
		return
	}
	c.kernel.EnableModel(name)
	fmt.Printf("%s %s enabled\n", paint(cGreen, "✓"), paint(cWhite, name))
}

// modelTest is the explicit "test connection" action (local-first upgrade
// §4c): unlike the auto-discovered fallback in modelAdd's fetch step, this
// runs synchronously and reports the actual error, so a user can verify an
// already-configured model on demand instead of only finding out a
// connection is broken when a real chat request fails. name is a key from
// c.cfg.Models, or empty to test the primary (c.cfg.Model).
func (c *Console) modelTest(args []string) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	var mc struct {
		provider, baseURL, apiKey, model string
	}
	if name == "" || name == c.cfg.Model {
		mc.provider, mc.baseURL, mc.apiKey, mc.model = c.cfg.Provider, c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Model
		name = c.cfg.Model
	} else if m, ok := c.cfg.Models[name]; ok {
		mc.provider, mc.baseURL, mc.apiKey, mc.model = m.Provider, m.BaseURL, m.APIKey, m.Model
	} else {
		fmt.Printf("%s no configured model named %q (see /models for the list)\n", paint(cRed, "✗"), name)
		return
	}
	if mc.baseURL == "" {
		fmt.Printf("%s %q has no base URL configured (local/embedded models aren't tested this way — see /local)\n", paint(cRed, "✗"), name)
		return
	}

	client := llm.NewClient(mc.baseURL, mc.apiKey, mc.model)
	client.SetProvider(mc.provider)

	fmt.Printf("%s testing connection to %s...\n", paint(cGray, "›"), paint(cWhite, name))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		fmt.Printf("%s %s: %v\n", paint(cRed, "✗ connection failed"), name, err)
		return
	}
	fmt.Printf("%s %s is reachable\n", paint(cGreen, "✓"), name)
}

func (c *Console) listModels() {
	total := len(c.cfg.Models) + 1 // +1 for the primary
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "REGISTERED MODELS"), paint(cGray, "("+fmtNum(total)+")"))
	primaryLabel := paint(cOrange, "primary")
	if c.kernel != nil && c.kernel.IsModelDisabled(c.cfg.Model) {
		primaryLabel = paint(cOrange, "primary") + paint(cRed, "  ⊘ disabled")
	}
	fmt.Printf("  %s  %s  %s  %s  %s\n",
		paint(cOrange, "★"),
		paint(cWhite, padRight(c.cfg.Model, 28)),
		paint(cBlue, padRight(config.ResolveTier(c.cfg.Provider, c.cfg.Model), 10)),
		paint(cGray, padRight(c.cfg.Provider, 14)),
		primaryLabel)
	for k, m := range c.cfg.Models {
		tier := m.Tier
		if tier == "" {
			tier = config.ResolveTier(m.Provider, m.Model)
		}
		marker := "•"
		markerColor := cGray
		if c.kernel != nil && c.kernel.IsModelDisabled(m.Model) {
			marker = "⊘ disabled"
			markerColor = cRed
		}
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			paint(markerColor, marker),
			paint(cWhite, padRight(k, 28)),
			paint(cBlue, padRight(tier, 10)),
			paint(cGray, padRight(m.Provider, 14)),
			paint(cGray, strutil.Truncate(m.BaseURL, 40)))
	}
	fmt.Printf("\n  %s /models add <provider> <model> [api_key]  ·  /providers to browse\n", paint(cGray, "add with:"))
}

func (c *Console) modelAdd(args []string) {
	if len(args) == 0 {
		// Interactive wizard — mirrors the GUI's provider-driven flow.
		c.modelAddWizard()
		return
	}
	if len(args) < 2 {
		fmt.Printf("%s usage: /models add <provider> <model> [api_key] [base_url]\n", paint(cRed, "✗"))
		fmt.Printf("   %s or run /models add with no args for the interactive wizard\n", paint(cGray, ""))
		fmt.Printf("   %s browse providers with /providers\n", paint(cGray, ""))
		return
	}
	provider := args[0]
	model := args[1]
	apiKey := ""
	if len(args) > 2 {
		apiKey = args[2]
	}
	baseURL := ""
	if len(args) > 3 {
		baseURL = args[3]
	}
	// Resolve base URL + auth from the provider registry if not supplied.
	if baseURL == "" {
		if p, ok := config.LookupProvider(provider); ok {
			baseURL = p.BaseURL
		}
	}
	// If no api key and provider is local (auth=none), leave empty.
	if apiKey == "" {
		if p, ok := config.LookupProvider(provider); ok && p.AuthScheme == config.AuthNone {
			// fine — local model needs no key
		}
	}
	if c.cfg.Models == nil {
		c.cfg.Models = make(map[string]config.ModelConfig)
	}
	tier := config.ResolveTier(provider, model)
	c.cfg.Models[model] = config.ModelConfig{
		Provider: provider,
		Model:    model,
		APIKey:   apiKey,
		BaseURL:  baseURL,
		Tier:     tier,
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	keyHint := paint(cGreen, "✓ key set")
	if apiKey == "" {
		keyHint = paint(cYellow, "no key (local?)")
	}
	fmt.Printf("%s registered %s  %s  %s  %s\n",
		paint(cGreen, "✓"),
		paint(cWhite, model),
		paint(cBlue, "("+tier+")"),
		paint(cGray, "("+provider+" @ "+baseURL+")"),
		keyHint)
}

// The models are fetched dynamically using the provider module

func (c *Console) modelAddWizard() {
	provs := config.Providers()

	// 1. Select Provider
	var providerItems []tui.SelectorItem
	for _, p := range provs {
		auth := p.AuthScheme
		if auth == config.AuthNone {
			auth = "local"
		}
		desc := fmt.Sprintf("%s · %d models", auth, len(p.Models))
		providerItems = append(providerItems, tui.SelectorItem{
			Title:       p.Name,
			Description: desc,
			Value:       p.ID,
		})
	}

	providerID := tui.Select("Select Provider:", providerItems)
	if providerID == "" {
		fmt.Println(paint(cGray, "Cancelled."))
		return
	}

	provider, ok := config.LookupProvider(providerID)
	if !ok {
		return
	}

	// 2. API key (skip for local/no-auth providers)
	apiKey := ""
	if provider.AuthScheme != config.AuthNone {
		if provider.KeyURL != "" {
			fmt.Printf("%s %s get a key at %s\n", paint(cGray, "│"), paint(cGray, "ℹ"), paint(cBlue, provider.KeyURL))
		}
		key, canceled := tui.Input("API Key (leave blank for none)", true)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		apiKey = strings.TrimSpace(key)
		if apiKey == "" {
			fmt.Printf("%s %s no key provided — model may fail to authenticate\n", paint(cGray, "│"), paint(cYellow, "⚠"))
		}
	}

	// 3. Optional Base URL for local/custom providers
	baseURL := provider.BaseURL
	if provider.Local || provider.CustomBaseURL {
		url, canceled := tui.Input("Base URL (optional, defaults to "+baseURL+")", false)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		if strings.TrimSpace(url) != "" {
			baseURL = strings.TrimSpace(url)
		}
	}

	// 4. Fetch models dynamically
	fmt.Printf("%s %s fetching active models from %s...\n", paint(cGray, "│"), paint(cBlue, "↻"), provider.ID)
	fetchedModels, err := provpkg.FetchModels(provider, apiKey, baseURL)

	var modelItems []tui.SelectorItem
	if err == nil && len(fetchedModels) > 0 {
		for _, m := range fetchedModels {
			desc := "Live fetched model"
			for _, km := range provider.Models {
				if km.ID == m {
					ctxK := fmtNum(km.ContextWindow)
					if km.ContextWindow >= 1000000 {
						ctxK = fmt.Sprintf("%.1fM", float64(km.ContextWindow)/1e6)
					}
					price := "free"
					if km.InputPrice > 0 || km.OutputPrice > 0 {
						price = fmt.Sprintf("$%.2f/$%.2f", km.InputPrice, km.OutputPrice)
					}
					desc = fmt.Sprintf("%s · %s ctx · %s", km.Tier, ctxK, price)
					break
				}
			}
			modelItems = append(modelItems, tui.SelectorItem{
				Title:       m,
				Description: desc,
				Value:       m,
			})
		}
	} else {
		fmt.Printf("%s %s failed to fetch dynamically, falling back to catalogue: %v\n", paint(cGray, "│"), paint(cYellow, "⚠"), err)
		for _, m := range provider.Models {
			ctxK := fmtNum(m.ContextWindow)
			if m.ContextWindow >= 1000000 {
				ctxK = fmt.Sprintf("%.1fM", float64(m.ContextWindow)/1e6)
			}
			price := "free"
			if m.InputPrice > 0 || m.OutputPrice > 0 {
				price = fmt.Sprintf("$%.2f/$%.2f", m.InputPrice, m.OutputPrice)
			}
			desc := fmt.Sprintf("%s · %s ctx · %s", m.Tier, ctxK, price)
			modelItems = append(modelItems, tui.SelectorItem{
				Title:       m.ID,
				Description: desc,
				Value:       m.ID,
			})
		}
	}
	modelItems = append(modelItems, tui.SelectorItem{
		Title:       "Custom Model...",
		Description: "Type a custom model ID",
		Value:       "__custom__",
	})

	// 5. Select Model
	modelID := tui.Select("Select Model:", modelItems)
	if modelID == "" {
		fmt.Println(paint(cGray, "Cancelled."))
		return
	}

	if modelID == "__custom__" {
		custom, canceled := tui.Input("Custom model ID", false)
		if canceled {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
		modelID = strings.TrimSpace(custom)
		if modelID == "" {
			fmt.Println(paint(cGray, "Cancelled."))
			return
		}
	}

	// Register
	if c.cfg.Models == nil {
		c.cfg.Models = make(map[string]config.ModelConfig)
	}
	tier := config.ResolveTier(provider.ID, modelID)
	c.cfg.Models[modelID] = config.ModelConfig{
		Provider: provider.ID,
		Model:    modelID,
		APIKey:   apiKey,
		BaseURL:  baseURL, // Use the dynamically updated one
		Tier:     tier,
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s %s\n", paint(cGray, "│"), paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)

	// Offer to set as primary
	ans, canceled := tui.Input("Set as primary? [Y/n]", false)
	if canceled {
		return
	}
	ans = strings.TrimSpace(strings.ToLower(ans))
	if ans == "" || ans == "y" || ans == "yes" {
		mc := c.cfg.Models[modelID]
		c.cfg.Model = mc.Model
		c.cfg.Provider = mc.Provider
		c.cfg.BaseURL = mc.BaseURL
		c.cfg.APIKey = mc.APIKey
		c.cfg.Save()
		c.kernel.ReloadModels(c.cfg)
		fmt.Printf("%s %s primary model → %s %s\n", paint(cGray, "│"), paint(cGreen, "✓"), paint(cOrange+clrBold, modelID), paint(cGray, "(hot-reloaded)"))
	}
	fmt.Printf("%s %s registered %s  %s  %s\n",
		paint(cGray, "│"),
		paint(cGreen, "✓"),
		paint(cWhite+clrBold, modelID),
		paint(cBlue, "("+tier+")"),
		paint(cGray, "("+provider.ID+")"))
}

func (c *Console) modelRemove(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models remove <model>\n", paint(cRed, "✗"))
		return
	}
	model := args[0]
	if _, ok := c.cfg.Models[model]; !ok {
		fmt.Printf("%s no such model: %s\n", paint(cRed, "✗"), model)
		return
	}
	delete(c.cfg.Models, model)
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s removed %s\n", paint(cGreen, "✓"), paint(cWhite, model))
}

func (c *Console) modelPrimary(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s usage: /models primary <model>\n", paint(cRed, "✗"))
		return
	}
	model := args[0]
	mc, ok := c.cfg.Models[model]
	if !ok {
		fmt.Printf("%s no such model: %s (add it first with /models add)\n", paint(cRed, "✗"), model)
		return
	}
	c.cfg.Model = mc.Model
	c.cfg.Provider = mc.Provider
	c.cfg.BaseURL = mc.BaseURL
	c.cfg.APIKey = mc.APIKey
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s primary model → %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, mc.Model), paint(cGray, "(hot-reloaded)"))
}

// handleProviders dispatches /providers subcommands.
func (c *Console) handleProviders(args []string) {
	if len(args) == 0 {
		c.listProviders()
		return
	}
	c.showProvider(args[0])
}

func (c *Console) listProviders() {
	provs := config.Providers()
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROVIDER CATALOGUE"), paint(cGray, "("+fmtNum(len(provs))+" providers)"))
	for _, p := range provs {
		auth := paint(cGreen, p.AuthScheme)
		if p.AuthScheme == config.AuthNone {
			auth = paint(cCyan, "local")
		}
		local := ""
		if p.Local {
			local = paint(cCyan, " (local)")
		}
		fmt.Printf("  %s  %s  %s  %s%s\n",
			paint(cOrange, padRight(p.ID, 14)),
			paint(cWhite, padRight(p.Name, 24)),
			auth,
			paint(cGray, fmt.Sprintf("%d models", len(p.Models))),
			local)
	}
	fmt.Printf("\n  %s /providers <id> to list models for a provider\n", paint(cGray, "e.g. /providers openai"))
}

func (c *Console) showProvider(id string) {
	p, ok := config.LookupProvider(id)
	if !ok {
		fmt.Printf("%s unknown provider: %s\n", paint(cRed, "✗"), id)
		return
	}
	fmt.Printf("%s  %s  %s\n", paint(cAmber+clrBold, p.Name), paint(cGray, "("+p.ID+")"), paint(cGray, p.BaseURL))
	fmt.Printf("  auth: %s  models: %s\n", paint(cGreen, p.AuthScheme), fmtNum(len(p.Models)))
	fmt.Println()
	for _, m := range p.Models {
		ctxK := fmtNum(m.ContextWindow)
		if m.ContextWindow >= 1000000 {
			ctxK = fmt.Sprintf("%.1fM", float64(m.ContextWindow)/1e6)
		}
		price := paint(cGreen, "free")
		if m.InputPrice > 0 || m.OutputPrice > 0 {
			price = paint(cGreen, fmt.Sprintf("$%.2f/$%.2f per 1M", m.InputPrice, m.OutputPrice))
		}
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			paint(cOrange, "•"),
			paint(cWhite, padRight(m.ID, 30)),
			paint(cBlue, padRight(m.Tier, 10)),
			paint(cGray, padRight(ctxK+" ctx", 12)),
			price)
		if m.Description != "" {
			fmt.Printf("     %s\n", paint(cGray, m.Description))
		}
	}
	fmt.Printf("\n  %s /models add %s <model-id> [api_key]\n", paint(cGray, "add with:"), paint(cOrange, p.ID))
}
