package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// BrowserTool provides headless browser capabilities via an embedded Python/Playwright script.
// It acts as a subagent for complex web interactions.
type BrowserTool struct{}

func NewBrowserTool() *BrowserTool {
	return &BrowserTool{}
}

const playwrightScript = `
import sys
import json
import asyncio
from playwright.async_api import async_playwright

async def run(url, action, selector, value):
    try:
        async with async_playwright() as p:
            browser = await p.chromium.launch(headless=True)
            page = await browser.new_page()
            await page.goto(url, wait_until="networkidle")
            
            result = {}
            if action == "read":
                content = await page.content()
                text = await page.evaluate('document.body.innerText')
                result = {"text": text[:50000]} # truncate to avoid overflow
            elif action == "click":
                await page.click(selector)
                await page.wait_for_timeout(2000)
                text = await page.evaluate('document.body.innerText')
                result = {"text": text[:50000]}
            elif action == "type":
                await page.fill(selector, value)
                await page.wait_for_timeout(1000)
                result = {"status": "success", "message": f"Typed '{value}' into {selector}"}
                
            await browser.close()
            print(json.dumps({"success": True, "data": result}))
    except Exception as e:
        print(json.dumps({"success": False, "error": str(e)}))

if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.exit(1)
    url = sys.argv[1]
    action = sys.argv[2]
    selector = sys.argv[3] if len(sys.argv) > 3 else ""
    value = sys.argv[4] if len(sys.argv) > 4 else ""
    asyncio.run(run(url, action, selector, value))
`

func (t *BrowserTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	url, _ := args["url"].(string)
	action, _ := args["action"].(string)
	selector, _ := args["selector"].(string)
	value, _ := args["value"].(string)

	if url == "" || action == "" {
		return &ToolResult{Name: "browser_subagent", Success: false, Error: "url and action are required"}
	}

	// Write the script to a temporary file
	tmpDir := os.TempDir()
	scriptPath := filepath.Join(tmpDir, "darkcode_browser.py")
	if err := os.WriteFile(scriptPath, []byte(playwrightScript), 0644); err != nil {
		return &ToolResult{Name: "browser_subagent", Success: false, Error: "failed to write browser script: " + err.Error()}
	}

	// Run the script
	cmd := exec.CommandContext(ctx, "python3", scriptPath, url, action, selector, value)
	output, err := cmd.CombinedOutput()
	
	if err != nil {
		return &ToolResult{
			Name:    "browser_subagent",
			Success: false,
			Error:   fmt.Sprintf("Browser error: %v, Output: %s. Note: ensure python3 and playwright are installed (pip install playwright && playwright install chromium).", err, string(output)),
		}
	}

	return &ToolResult{
		Name:    "browser_subagent",
		Success: true,
		Output:  string(output),
	}
}

func (t *BrowserTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL to visit"},
			"action": {
				"type": "string",
				"enum": ["read", "click", "type"],
				"description": "Action to perform in the browser"
			},
			"selector": {"type": "string", "description": "CSS selector for click/type actions"},
			"value": {"type": "string", "description": "Value to input for type action"}
		},
		"required": ["url", "action"]
	}`
}
