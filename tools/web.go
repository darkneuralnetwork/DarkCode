package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/safeurl"
)

// WebTool fetches web content via HTTP GET and orchestrates intelligent web searches.
type WebTool struct {
	HTTPClient *http.Client
	Registry   *Registry
	Router     core.ModelRouter
}

func NewWebTool(registry *Registry, router core.ModelRouter) *WebTool {
	return &WebTool{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Registry:   registry,
		Router:     router,
	}
}

// FetchURL retrieves content from a URL.
func (t *WebTool) FetchURL(ctx context.Context, args map[string]interface{}) *ToolResult {
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Name: "web_fetch", Success: false, Error: "url is required"}
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	// SSRF guard: block loopback, link-local (cloud metadata), and private
	// ranges so the agent cannot be directed at internal services.
	if !safeurl.IsSafeFetchURL(url, false) {
		return &ToolResult{Name: "web_fetch", Success: false, Error: "blocked: url targets a loopback, link-local, or private address (SSRF guard)"}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return &ToolResult{Name: "web_fetch", Success: false, Error: err.Error()}
	}
	req.Header.Set("User-Agent", "DarkCode/1.0")

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return &ToolResult{Name: "web_fetch", Success: false, Error: err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 500000)) // 500KB limit
	if err != nil {
		return &ToolResult{Name: "web_fetch", Success: false, Error: err.Error()}
	}

	content := string(body)
	// Truncate for context management
	if len(content) > 50000 {
		content = content[:50000] + "\n... (truncated)"
	}

	return &ToolResult{
		Name:    "web_fetch",
		Success: true,
		Output:  fmt.Sprintf("Status: %d\nURL: %s\n\n%s", resp.StatusCode, resp.Request.URL.String(), content),
	}
}

// WebSearch performs a basic web search using Wikipedia API as a stable fallback.
// DuckDuckGo Lite HTML scraping is highly brittle and often returns 403 Forbidden.
// WebSearch performs an intelligent web search by first classifying the user's intent,
// dispatching to the appropriate local or remote source, and then summarizing the context.
func (t *WebTool) WebSearch(ctx context.Context, args map[string]interface{}) *ToolResult {
	query, _ := args["query"].(string)
	if query == "" {
		return &ToolResult{Name: "web_search", Success: false, Error: "query is required"}
	}

	intent := "web"
	var client core.LLMClient
	if t.Router != nil {
		// Use TierFast for lightweight planning and summarization
		client, _, _ = t.Router.Route(core.ModelTierFast, 1, "plan web search intent")
	}

	if client != nil {
		if lm, ok := client.(core.LoRAManager); ok {
			_ = lm.MountLoRA("planner", 1.0)
			defer lm.MountLoRA("planner", 0.0)
		}
		maxTokens10 := 10
		resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
			Messages: []core.Message{
				{
					Role:    core.RoleSystem,
					Content: "You are an intelligent retrieval planner. Determine the best search source for the user's query:\n1. 'local' (for code, architecture, functions in current repo)\n2. 'docs' (for framework usage, SDKs, APIs)\n3. 'github' (for examples, implementations, open source projects)\n4. 'web' (for general info, news, concepts)\nReturn ONLY the single word (local, docs, github, or web).",
				},
				{
					Role:    core.RoleUser,
					Content: fmt.Sprintf("Query: '%s'", query),
				},
			},
			MaxTokens: &maxTokens10,
		})
		if err == nil && len(resp.Choices) > 0 {
			content := strings.ToLower(strings.TrimSpace(resp.Choices[0].Message.Content))
			if strings.Contains(content, "local") {
				intent = "local"
			} else if strings.Contains(content, "github") {
				intent = "github"
			} else if strings.Contains(content, "docs") {
				intent = "docs"
			}
		}
	}

	var rawResult string
	switch intent {
	case "local":
		rawResult = t.searchLocal(ctx, query)
	case "github":
		rawResult = t.searchGitHub(ctx, query)
	default:
		rawResult = t.searchWikipedia(ctx, query)
	}

	if client != nil && len(rawResult) > 500 && !strings.HasPrefix(rawResult, "Error:") && !strings.Contains(rawResult, "unavailable") {
		if lm, ok := client.(core.LoRAManager); ok {
			_ = lm.MountLoRA("summarizer", 1.0)
			defer lm.MountLoRA("summarizer", 0.0)
		}
		maxTokens2000 := 2000
		summaryMsg := core.Message{
			Role: "user",
			Content: fmt.Sprintf("Extract the most relevant information from these raw retrieval results to answer the query: '%s'. Remove duplicates, trim unnecessary text, and produce a concise, high-signal context package.\n\nRaw Results:\n%s", query, rawResult),
		}
		resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
			Messages: []core.Message{summaryMsg},
			MaxTokens: &maxTokens2000,
		})
		if err == nil && len(resp.Choices) > 0 {
			return &ToolResult{
				Name:    "web_search",
				Success: true,
				Output:  fmt.Sprintf("[Source: %s]\n\n%s", intent, resp.Choices[0].Message.Content),
			}
		}
	}

	if len(rawResult) > 4000 {
		rawResult = rawResult[:4000] + "\n... (truncated for context limits)"
	}

	return &ToolResult{
		Name:    "web_search",
		Success: true,
		Output:  fmt.Sprintf("[Source: %s]\n\n%s", intent, rawResult),
	}
}

func (t *WebTool) searchLocal(ctx context.Context, query string) string {
	if t.Registry == nil {
		return "Local search unavailable (no registry injected)"
	}
	tool, ok := t.Registry.Get("search_files")
	if !ok {
		return "Local search unavailable (search_files tool missing)"
	}
	res := tool.Handler(ctx, map[string]interface{}{"pattern": query})
	if !res.Success {
		return "Local search failed: " + res.Error
	}
	if res.Output == "" {
		return "No local matches found."
	}
	return res.Output
}

func (t *WebTool) searchGitHub(ctx context.Context, query string) string {
	searchURL := "https://api.github.com/search/repositories?q=" + neturl.QueryEscape(query) + "&sort=stars&order=desc"
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "Error creating request: " + err.Error()
	}
	req.Header.Set("User-Agent", "DarkCode-Bot/1.0")
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return "Error fetching GitHub: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "Error reading GitHub response: " + err.Error()
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "Error parsing GitHub response: " + err.Error()
	}

	items, ok := result["items"].([]interface{})
	if !ok || len(items) == 0 {
		return "No repositories found on GitHub."
	}

	var out strings.Builder
	for i, item := range items {
		if i >= 5 {
			break
		}
		repo := item.(map[string]interface{})
		fullName, _ := repo["full_name"].(string)
		desc, _ := repo["description"].(string)
		url, _ := repo["html_url"].(string)
		stars, _ := repo["stargazers_count"].(float64)
		out.WriteString(fmt.Sprintf("- %s (%.0f stars): %s\n  URL: %s\n\n", fullName, stars, desc, url))
	}
	return out.String()
}

func (t *WebTool) searchWikipedia(ctx context.Context, query string) string {
	searchURL := "https://en.wikipedia.org/w/api.php?action=query&list=search&srsearch=" + neturl.QueryEscape(query) + "&utf8=&format=json"

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "Error: " + err.Error()
	}
	req.Header.Set("User-Agent", "DarkCode-Bot/1.0 (https://github.com/darkcode)")

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return "Error: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "Error: " + err.Error()
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "Error failed to parse search results: " + err.Error()
	}

	var out strings.Builder
	queryMap, ok := result["query"].(map[string]interface{})
	if !ok {
		return "No results found."
	}
	
	searchList, ok := queryMap["search"].([]interface{})
	if !ok || len(searchList) == 0 {
		return "No results found."
	}

	for i, item := range searchList {
		if i >= 5 {
			break
		}
		itemMap := item.(map[string]interface{})
		title, _ := itemMap["title"].(string)
		snippet, _ := itemMap["snippet"].(string)
		snippet = strings.ReplaceAll(snippet, "<span class=\"searchmatch\">", "")
		snippet = strings.ReplaceAll(snippet, "</span>", "")
		snippet = strings.ReplaceAll(snippet, "&quot;", "\"")
		
		out.WriteString(fmt.Sprintf("- %s: %s\n\n", title, snippet))
	}

	return out.String()
}

// extractTags is a simple HTML extractor for DDG snippets (kept for backward compatibility or other tools)
func extractTags(html string, classMatch string, limit int) []string {
	var results []string
	searchStr := "class='" + classMatch + "'"
	idx := 0
	for len(results) < limit {
		start := strings.Index(html[idx:], searchStr)
		if start == -1 {
			break
		}
		start += idx
		// find closing tag
		closeTag := strings.Index(html[start:], "</td>")
		if closeTag == -1 {
			break
		}
		closeTag += start
		
		// find start of content
		contentStart := strings.Index(html[start:closeTag], ">")
		if contentStart == -1 {
			idx = closeTag
			continue
		}
		contentStart += start + 1
		
		text := html[contentStart:closeTag]
		text = strings.ReplaceAll(text, "<b>", "")
		text = strings.ReplaceAll(text, "</b>", "")
		results = append(results, strings.TrimSpace(text))
		idx = closeTag
	}
	return results
}


// WebResult is a helper for error results.
type WebResult struct{ err error }

func (w *WebResult) Result() *ToolResult {
	return &ToolResult{Name: "web_search", Success: false, Error: w.err.Error()}
}

func (t *WebTool) FetchSchema() string {
	return `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`
}

func (t *WebTool) SearchSchema() string {
	return `{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"}
		},
		"required": ["query"]
	}`
}
