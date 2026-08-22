package tools

import (
	"context"
	"fmt"

	"github.com/darkcode/core"
	"github.com/darkcode/project"
)

// ProjectTool handles interaction with the project plan and workflow.
type ProjectTool struct {
	store *project.Store
}

// NewProjectTool creates a new project tool handler.
func NewProjectTool(store *project.Store) *ProjectTool {
	return &ProjectTool{store: store}
}

func getProjectID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(core.ProjectKey).(string); ok {
		return id
	}
	return ""
}

// UpdatePlanSchema returns the JSON schema for updating the plan.
func (p *ProjectTool) UpdatePlanSchema() string {
	return `{
		"type": "object",
		"properties": {
			"plan_markdown": {
				"type": "string",
				"description": "The full markdown content of the new implementation plan."
			}
		},
		"required": ["plan_markdown"]
	}`
}

// UpdatePlan executes the update plan tool.
func (p *ProjectTool) UpdatePlan(ctx context.Context, args map[string]interface{}) *ToolResult {
	if p.store == nil {
		return &ToolResult{Error: "project store is not initialized"}
	}
	projID := getProjectID(ctx)
	if projID == "" {
		return &ToolResult{Error: "no active project in context"}
	}

	planMarkdown, _ := args["plan_markdown"].(string)

	err := p.store.SetPlan(projID, planMarkdown)
	if err != nil {
		return &ToolResult{Error: err.Error()}
	}

	return &ToolResult{Output: "Implementation plan updated successfully."}
}

// UpdateWorkflowSchema returns the JSON schema for updating the workflow.
func (p *ProjectTool) UpdateWorkflowSchema() string {
	return `{
		"type": "object",
		"properties": {
			"workflow_markdown": {
				"type": "string",
				"description": "The full markdown content of the new workflow architecture."
			}
		},
		"required": ["workflow_markdown"]
	}`
}

// UpdateWorkflow executes the update workflow tool.
func (p *ProjectTool) UpdateWorkflow(ctx context.Context, args map[string]interface{}) *ToolResult {
	if p.store == nil {
		return &ToolResult{Error: "project store is not initialized"}
	}
	projID := getProjectID(ctx)
	if projID == "" {
		return &ToolResult{Error: "no active project in context"}
	}

	workflowMarkdown, _ := args["workflow_markdown"].(string)

	err := p.store.SetWorkflow(projID, workflowMarkdown)
	if err != nil {
		return &ToolResult{Error: err.Error()}
	}

	return &ToolResult{Output: "Workflow architecture updated successfully."}
}

// ReadProjectContextSchema returns the JSON schema for reading the current project context.
func (p *ProjectTool) ReadProjectContextSchema() string {
	return `{
		"type": "object",
		"properties": {},
		"required": []
	}`
}

// ReadProjectContext executes the tool.
func (p *ProjectTool) ReadProjectContext(ctx context.Context, args map[string]interface{}) *ToolResult {
	if p.store == nil {
		return &ToolResult{Error: "project store is not initialized"}
	}
	projID := getProjectID(ctx)
	if projID == "" {
		return &ToolResult{Error: "no active project in context"}
	}

	plan, _ := p.store.GetPlan(projID)
	workflow, _ := p.store.GetWorkflow(projID)

	result := fmt.Sprintf("## Current Implementation Plan\n%s\n\n## Current Workflow Architecture\n%s\n", plan, workflow)
	return &ToolResult{Output: result}
}

// RegisterProjectTools registers the project management tools.
func RegisterProjectTools(registry *Registry, store *project.Store) {
	if store == nil {
		return
	}
	p := NewProjectTool(store)

	registry.Register(&ToolEntry{
		Name:        "update_project_plan",
		Description: "Update the active project's implementation plan. Completely replaces the existing plan with the provided markdown.",
		Parameters:  MustParseSchema(p.UpdatePlanSchema()),
		Handler:     p.UpdatePlan,
		Category:    "planning",
	})

	registry.Register(&ToolEntry{
		Name:        "update_project_workflow",
		Description: "Update the active project's workflow architecture. Completely replaces the existing workflow with the provided markdown.",
		Parameters:  MustParseSchema(p.UpdateWorkflowSchema()),
		Handler:     p.UpdateWorkflow,
		Category:    "planning",
	})

	registry.Register(&ToolEntry{
		Name:        "read_project_context",
		Description: "Read the current implementation plan and workflow architecture for the active project.",
		Parameters:  MustParseSchema(p.ReadProjectContextSchema()),
		Handler:     p.ReadProjectContext,
		Category:    "planning",
	})
}
