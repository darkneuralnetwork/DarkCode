package agents

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/ui"
)

// ============================================================================
// SELF-VERIFICATION PIPELINE (Multi-Stage)
//
// Every critical output must pass multiple stages before being accepted:
//   1. Syntax Check  — instant, deterministic tree-sitter parse
//   2. Format Check  — gofmt / prettier / black
//   3. Lint          — golangci-lint / eslint
//   4. Type Check    — go build / tsc
//   5. Test Check    — go test / jest
//   6. Semantic Check — fallback to LLM verification
// ============================================================================

type VerificationStage interface {
	Name() string
	Verify(ctx context.Context, goal, output string) (*core.VerificationResult, error)
	IsApplicable(output string) bool
}

// VerificationPipeline validates agent outputs before they are finalized.
type VerificationPipeline struct {
	router              core.ModelRouter
	emitter             *ui.EventEmitter
	confidenceThreshold float64
	stages              []VerificationStage
}

// CmdVerificationStage executes a shell command to verify the project state.
type CmdVerificationStage struct {
	name string
	cmd  string
	args []string
}

func (c *CmdVerificationStage) Name() string { return c.name }
func (c *CmdVerificationStage) IsApplicable(output string) bool { return true }
func (c *CmdVerificationStage) Verify(ctx context.Context, goal, output string) (*core.VerificationResult, error) {
	cmd := exec.CommandContext(ctx, c.cmd, c.args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &core.VerificationResult{Passed: false, Confidence: core.ConfidenceScore{Overall: 0.0}, Issues: []string{string(out)}}, nil
	}
	return &core.VerificationResult{Passed: true, Confidence: core.ConfidenceScore{Overall: 1.0}}, nil
}

// NewVerificationPipeline creates a new verification pipeline. workspace is
// the project root (used for language detection and stage applicability).
func NewVerificationPipeline(rtr core.ModelRouter, emitter *ui.EventEmitter, workspace string) *VerificationPipeline {
	return &VerificationPipeline{
		router:              rtr,
		emitter:             emitter,
		confidenceThreshold: 0.6,
		stages: []VerificationStage{
			NewGoStage("formatter", "gofmt", []string{"-l", "."}, workspace),
			NewGoStage("compiler", "go", []string{"build", "./..."}, workspace),
			NewGoStage("tests", "go", []string{"test", "./..."}, workspace),
			NewGoStage("lint", "go", []string{"vet", "./..."}, workspace),
			NewSecurityScanStage(workspace),
			NewStyleCheckStage(workspace),
			NewPatchValidationStage(workspace),
		},
	}
}

// SetThreshold sets the minimum confidence score to pass verification.
func (v *VerificationPipeline) SetThreshold(t float64) {
	v.confidenceThreshold = t
}

// Verify runs the full multi-stage verification pipeline on an output.
func (v *VerificationPipeline) Verify(ctx context.Context, goal, output string, toolsUsed []string) (*core.VerificationResult, error) {
	result := &core.VerificationResult{
		VerifiedAt: time.Now(),
		Passed:     true, // assume pass until proven otherwise
		Confidence: core.ConfidenceScore{Overall: 1.0, Evidence: 1.0, Verification: 1.0}, // assume high confidence if deterministic
	}

	for _, stage := range v.stages {
		if stage.IsApplicable(output) {
			if v.emitter != nil {
				v.emitter.EmitTaskUpdate("verification", "running", fmt.Sprintf("Running verification stage: %s", stage.Name()))
			}
			stageResult, err := stage.Verify(ctx, goal, output)
			if err != nil {
				if v.emitter != nil {
					v.emitter.EmitTaskUpdate("verification", "error", fmt.Sprintf("Stage %s failed: %v", stage.Name(), err))
				}
				result.Issues = append(result.Issues, fmt.Sprintf("%s failed: %v", stage.Name(), err))
				result.Passed = false
			}
			if stageResult != nil {
				result.Issues = append(result.Issues, stageResult.Issues...)
				if !stageResult.Passed {
					if v.emitter != nil {
						v.emitter.EmitTaskUpdate("verification", "failed", fmt.Sprintf("Stage %s failed checks", stage.Name()))
					}
					result.Passed = false
				} else {
					if v.emitter != nil {
						v.emitter.EmitTaskUpdate("verification", "passed", fmt.Sprintf("Stage %s passed successfully", stage.Name()))
					}
				}
				if stageResult.Confidence.Overall < result.Confidence.Overall {
					result.Confidence = stageResult.Confidence
				}
			}
		}
	}

	// If no stages failed, it's verified!
	// (In a full implementation, we'd fallback to LLM verification if no deterministic stages applied).
	if !result.Passed {
		result.Issues = append(result.Issues, "Failed one or more verification stages.")
	}

	return result, nil
}

// QuickVerify runs a fast verification. In the deterministic pipeline, this is identical to Verify.
func (v *VerificationPipeline) QuickVerify(ctx context.Context, goal, output string) (*core.VerificationResult, error) {
	return v.Verify(ctx, goal, output, nil)
}
