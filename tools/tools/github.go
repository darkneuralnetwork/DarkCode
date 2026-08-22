package tools

// github.go — GitHub API access for the PR-review and issue-triage workflows.
//
// Uses the REST API over stdlib net/http; no SDK. The token is read from
// GITHUB_TOKEN / GH_TOKEN, falling back to the `gh` CLI's stored credential so
// an already-authenticated machine needs no extra setup.
//
// Read actions are free. Every action that writes to GitHub — posting a review
// or a comment — is routed through the permission gate like any other mutating
// tool, because it publishes under the user's identity.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/darkcode/infra/safeurl"
	"github.com/darkcode/infra/security"
)

// GitHubTool talks to the GitHub REST API for the active repository.
type GitHubTool struct {
	HTTPClient *http.Client
	Workspace  string

	// APIBase is the REST root; overridden in tests and for GitHub Enterprise.
	APIBase string
}

func NewGitHubTool(workspace string) *GitHubTool {
	return &GitHubTool{
		HTTPClient: safeurl.EgressClient(30 * time.Second),
		Workspace:  workspace,
		APIBase:    "https://api.github.com",
	}
}

// token resolves a credential, preferring the environment over the gh CLI.
func (t *GitHubTool) token() string {
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	cmd := exec.Command("gh", "auth", "token")
	cmd.Dir = t.Workspace
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// remoteRepo matches the owner/name of a GitHub remote in either URL form.
var remoteRepo = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?$`)

// repo returns "owner/name" for the workspace's origin remote.
func (t *GitHubTool) repo(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = t.Workspace
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("no git origin remote found; pass repo as \"owner/name\"")
	}
	m := remoteRepo.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("origin remote is not a GitHub repository")
	}
	return m[1] + "/" + m[2], nil
}

// call performs one API request. A nil body means GET.
func (t *GitHubTool) call(ctx context.Context, method, path string, body interface{}, accept string) (string, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.APIBase+path, reader)
	if err != nil {
		return "", err
	}
	if tok := t.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			msg += " (set GITHUB_TOKEN or run `gh auth login`)"
		}
		return "", fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, msg)
	}
	return string(raw), nil
}

func (t *GitHubTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	fail := func(format string, a ...interface{}) *ToolResult {
		return &ToolResult{Name: "github", Success: false, Error: fmt.Sprintf(format, a...)}
	}
	action, _ := args["action"].(string)
	repo, err := t.repo(str(args["repo"]))
	if err != nil {
		return fail("%v", err)
	}
	number := ""
	if n, ok := args["number"].(float64); ok {
		number = fmt.Sprintf("%d", int(n))
	}
	needNumber := func() bool { return number != "" }

	const jsonAccept = "application/vnd.github+json"
	var out string

	switch action {
	case "pr_list":
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/pulls?state=open&per_page=30", nil, jsonAccept)

	case "pr_get":
		if !needNumber() {
			return fail("pr_get requires number")
		}
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/pulls/"+number, nil, jsonAccept)

	case "pr_diff":
		if !needNumber() {
			return fail("pr_diff requires number")
		}
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/pulls/"+number, nil, "application/vnd.github.v3.diff")

	case "pr_review":
		if !needNumber() {
			return fail("pr_review requires number")
		}
		body := str(args["body"])
		if body == "" {
			return fail("pr_review requires body")
		}
		event := strings.ToUpper(str(args["event"]))
		if event == "" {
			event = "COMMENT"
		}
		switch event {
		case "COMMENT", "APPROVE", "REQUEST_CHANGES":
		default:
			return fail("event must be COMMENT, APPROVE, or REQUEST_CHANGES")
		}
		out, err = t.call(ctx, "POST", "/repos/"+repo+"/pulls/"+number+"/reviews",
			map[string]string{"body": body, "event": event}, jsonAccept)

	case "issue_list":
		state := str(args["state"])
		if state == "" {
			state = "open"
		}
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/issues?state="+state+"&per_page=30", nil, jsonAccept)

	case "issue_get":
		if !needNumber() {
			return fail("issue_get requires number")
		}
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/issues/"+number, nil, jsonAccept)

	case "issue_comment":
		if !needNumber() {
			return fail("issue_comment requires number")
		}
		body := str(args["body"])
		if body == "" {
			return fail("issue_comment requires body")
		}
		out, err = t.call(ctx, "POST", "/repos/"+repo+"/issues/"+number+"/comments",
			map[string]string{"body": body}, jsonAccept)

	case "checks":
		ref := str(args["ref"])
		if ref == "" {
			ref = "HEAD"
		}
		out, err = t.call(ctx, "GET", "/repos/"+repo+"/commits/"+ref+"/check-runs", nil, jsonAccept)

	default:
		return fail("unknown action %q (want: pr_list, pr_get, pr_diff, pr_review, issue_list, issue_get, issue_comment, checks)", action)
	}

	if err != nil {
		return fail("%v", err)
	}
	// PR and issue bodies are user-supplied text from the internet, so they
	// get the same injection scan as a fetched page.
	return &ToolResult{Name: "github", Success: true, Output: security.Wrap("github:"+repo, out)}
}

// RegisterGitHubTool adds the GitHub tool to the registry.
func RegisterGitHubTool(r *Registry, workspace string) {
	t := NewGitHubTool(workspace)
	r.Register(&ToolEntry{
		Name: "github",
		Description: strings.TrimSpace(`
Read and act on GitHub pull requests, issues and CI checks for this repository.
Read actions: pr_list, pr_get, pr_diff, issue_list, issue_get, checks.
Write actions (these publish under the user's account and require approval): pr_review, issue_comment.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["pr_list", "pr_get", "pr_diff", "pr_review", "issue_list", "issue_get", "issue_comment", "checks"], "description": "Which operation to perform"},
				"number": {"type": "integer", "description": "Pull request or issue number"},
				"body": {"type": "string", "description": "Markdown body for pr_review or issue_comment"},
				"event": {"type": "string", "enum": ["COMMENT", "APPROVE", "REQUEST_CHANGES"], "description": "Review verdict for pr_review (default COMMENT)"},
				"state": {"type": "string", "enum": ["open", "closed", "all"], "description": "Filter for issue_list (default open)"},
				"ref": {"type": "string", "description": "Commit ref for checks (default HEAD)"},
				"repo": {"type": "string", "description": "owner/name, when not the workspace's origin remote"}
			},
			"required": ["action"]
		}`),
		Handler:  t.Execute,
		Category: "vcs",
		// Not read-only: two of its actions publish to GitHub. The permission
		// gate distinguishes them per call.
		ReadOnly: false,
	})
}
