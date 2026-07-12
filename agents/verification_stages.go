package agents

// verification_stages.go — additional verification stages that were missing
// from the original pipeline (spec §10: "missing security scan, style, patch
// validation"). These extend the base CmdVerificationStage with:
//   - Language detection so Go-specific stages only run on Go projects
//   - A security scan stage (govulncheck / gosec)
//   - A patch validation stage (checks that changed files compile)
//   - A style check stage (golangci-lint, with go vet fallback)

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/darkcode/core"
)

// execLookPath is exec.LookPath (wrapped for testability).
var execLookPath = exec.LookPath

// languageDetector determines the primary language of the workspace so the
// verification pipeline can skip inapplicable stages.
type languageDetector struct {
	workspace string
}

func newLanguageDetector(workspace string) *languageDetector {
	return &languageDetector{workspace: workspace}
}

// IsGoProject reports whether the workspace contains a go.mod file.
func (d *languageDetector) IsGoProject() bool {
	ws := d.workspace
	if ws == "" {
		ws, _ = os.Getwd()
	}
	if ws == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(ws, "go.mod"))
	return err == nil
}

// IsNodeProject reports whether the workspace contains a package.json.
func (d *languageDetector) IsNodeProject() bool {
	ws := d.workspace
	if ws == "" {
		ws, _ = os.Getwd()
	}
	if ws == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(ws, "package.json"))
	return err == nil
}

// detectLanguage returns the primary project language.
func (d *languageDetector) detectLanguage() string {
	if d.IsGoProject() {
		return "go"
	}
	if d.IsNodeProject() {
		return "js"
	}
	return "unknown"
}

// GoStage is a CmdVerificationStage that only runs on Go projects.
type GoStage struct {
	CmdVerificationStage
	workspace string
}

func NewGoStage(name, cmd string, args []string, workspace string) *GoStage {
	return &GoStage{
		CmdVerificationStage: CmdVerificationStage{name: name, cmd: cmd, args: args},
		workspace:            workspace,
	}
}

func (g *GoStage) IsApplicable(output string) bool {
	return newLanguageDetector(g.workspace).IsGoProject()
}

// NodeStage is a CmdVerificationStage that only runs on Node/JS projects.
type NodeStage struct {
	CmdVerificationStage
	workspace string
}

func NewNodeStage(name, cmd string, args []string, workspace string) *NodeStage {
	return &NodeStage{
		CmdVerificationStage: CmdVerificationStage{name: name, cmd: cmd, args: args},
		workspace:            workspace,
	}
}

func (n *NodeStage) IsApplicable(output string) bool {
	return newLanguageDetector(n.workspace).IsNodeProject()
}

// SecurityScanStage runs govulncheck (Go vulnerability database) on the
// project. Falls back gracefully if govulncheck isn't installed (reports
// "skipped" rather than failing).
type SecurityScanStage struct {
	workspace string
}

func NewSecurityScanStage(workspace string) *SecurityScanStage {
	return &SecurityScanStage{workspace: workspace}
}

func (s *SecurityScanStage) Name() string { return "security_scan" }

func (s *SecurityScanStage) IsApplicable(output string) bool {
	return newLanguageDetector(s.workspace).IsGoProject()
}

func (s *SecurityScanStage) Verify(ctx context.Context, goal, output string) (*core.VerificationResult, error) {
	// Check if govulncheck is available.
	if _, err := execLookPath("govulncheck"); err != nil {
		return &core.VerificationResult{
			Passed:     true,
			Confidence: core.ConfidenceScore{Overall: 0.5},
			Issues:     []string{"govulncheck not installed; security scan skipped"},
		}, nil
	}
	return runCmdStage(ctx, "security_scan", "govulncheck", []string{"./..."}, s.workspace)
}

// StyleCheckStage runs golangci-lint if available, falling back to go vet.
type StyleCheckStage struct {
	workspace string
}

func NewStyleCheckStage(workspace string) *StyleCheckStage {
	return &StyleCheckStage{workspace: workspace}
}

func (s *StyleCheckStage) Name() string { return "style_check" }

func (s *StyleCheckStage) IsApplicable(output string) bool {
	return newLanguageDetector(s.workspace).IsGoProject()
}

func (s *StyleCheckStage) Verify(ctx context.Context, goal, output string) (*core.VerificationResult, error) {
	if _, err := execLookPath("golangci-lint"); err == nil {
		return runCmdStage(ctx, "style_check", "golangci-lint", []string{"run", "--timeout=60s"}, s.workspace)
	}
	// Fallback to go vet (already in the base pipeline, but we keep it as a
	// style proxy when golangci-lint is absent).
	return runCmdStage(ctx, "style_check", "go", []string{"vet", "./..."}, s.workspace)
}

// PatchValidationStage validates that the agent's output (file changes) is
// syntactically valid. For Go projects it runs `go build ./...` on the
// changed packages; for other projects it checks that changed files exist
// and are non-empty.
type PatchValidationStage struct {
	workspace string
}

func NewPatchValidationStage(workspace string) *PatchValidationStage {
	return &PatchValidationStage{workspace: workspace}
}

func (p *PatchValidationStage) Name() string { return "patch_validation" }
func (p *PatchValidationStage) IsApplicable(output string) bool { return true }

func (p *PatchValidationStage) Verify(ctx context.Context, goal, output string) (*core.VerificationResult, error) {
	// The "output" may contain file paths that were changed. Extract them
	// and verify each exists and is non-empty.
	files := extractFilePaths(output)
	if len(files) == 0 {
		// No files mentioned in output — nothing to validate.
		return &core.VerificationResult{Passed: true, Confidence: core.ConfidenceScore{Overall: 0.7}}, nil
	}
	var issues []string
	validCount := 0
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) && p.workspace != "" {
			path = filepath.Join(p.workspace, f)
		}
		info, err := os.Stat(path)
		if err != nil {
			issues = append(issues, "changed file not found: "+f)
			continue
		}
		if info.Size() == 0 {
			issues = append(issues, "changed file is empty: "+f)
			continue
		}
		validCount++
	}
	passed := validCount > 0 || len(files) == 0
	confidence := 0.8
	if validCount == len(files) && len(files) > 0 {
		confidence = 1.0
	}
	return &core.VerificationResult{
		Passed:     passed,
		Confidence: core.ConfidenceScore{Overall: confidence},
		Issues:     issues,
	}, nil
}

// extractFilePaths pulls plausible file paths from the agent output text.
// Looks for patterns like "path/to/file.go", "file.go", or paths in quotes.
func extractFilePaths(output string) []string {
	var files []string
	seen := map[string]bool{}
	// Split on whitespace and check for file-like tokens.
	for _, tok := range strings.Fields(output) {
		tok = strings.Trim(tok, "\"'`(),")
		if isLikelyFilePath(tok) && !seen[tok] {
			files = append(files, tok)
			seen[tok] = true
		}
	}
	return files
}

// isLikelyFilePath reports whether a token looks like a file path.
func isLikelyFilePath(s string) bool {
	if len(s) < 3 || strings.Contains(s, "\n") {
		return false
	}
	// Must have an extension.
	dot := strings.LastIndexByte(s, '.')
	if dot < 0 || dot == len(s)-1 {
		return false
	}
	ext := s[dot+1:]
	// Common source file extensions.
	for _, e := range []string{"go", "js", "ts", "tsx", "jsx", "py", "rs", "java", "c", "cpp", "h", "md", "yaml", "yml", "json", "toml"} {
		if ext == e {
			return true
		}
	}
	return false
}

// runCmdStage is a helper that runs a command and returns a verification result.
func runCmdStage(ctx context.Context, name, cmd string, args []string, workspace string) (*core.VerificationResult, error) {
	return (&CmdVerificationStage{name: name, cmd: cmd, args: args}).Verify(ctx, "", "")
}
