package tools

import (
	"context"
	"fmt"
	"sync"
)

// TodoItem represents a single task in the todo list.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed, cancelled
}

// TodoTool manages an in-session task list.
type TodoTool struct {
	mu    sync.Mutex
	items []TodoItem
}

func NewTodoTool() *TodoTool {
	return &TodoTool{items: make([]TodoItem, 0)}
}

func (t *TodoTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	itemsRaw, ok := args["todos"]
	if !ok {
		// Return current list
		return &ToolResult{Name: "todo", Success: true, Output: t.formatList()}
	}

	// Parse the todos array
	itemsRawArr, ok := itemsRaw.([]interface{})
	if !ok {
		return &ToolResult{Name: "todo", Success: false, Error: "todos must be an array"}
	}

	// Check for merge mode
	merge, _ := args["merge"].(bool)
	if merge {
		return t.mergeItems(itemsRawArr)
	}

	// Replace mode (default)
	t.items = make([]TodoItem, 0, len(itemsRawArr))
	for _, raw := range itemsRawArr {
		itemMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := itemMap["id"].(string)
		content, _ := itemMap["content"].(string)
		status, _ := itemMap["status"].(string)
		if status == "" {
			status = "pending"
		}
		t.items = append(t.items, TodoItem{ID: id, Content: content, Status: status})
	}

	return &ToolResult{Name: "todo", Success: true, Output: t.formatList()}
}

func (t *TodoTool) mergeItems(itemsRawArr []interface{}) *ToolResult {
	for _, raw := range itemsRawArr {
		itemMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := itemMap["id"].(string)
		content, _ := itemMap["content"].(string)
		status, _ := itemMap["status"].(string)

		found := false
		for i, existing := range t.items {
			if existing.ID == id {
				if content != "" {
					t.items[i].Content = content
				}
				if status != "" {
					t.items[i].Status = status
				}
				found = true
				break
			}
		}
		if !found && content != "" {
			if status == "" {
				status = "pending"
			}
			t.items = append(t.items, TodoItem{ID: id, Content: content, Status: status})
		}
	}
	return &ToolResult{Name: "todo", Success: true, Output: t.formatList()}
}

func (t *TodoTool) formatList() string {
	if len(t.items) == 0 {
		return "Todo list is empty."
	}
	var result string
	result = fmt.Sprintf("Todo List (%d items):\n", len(t.items))
	for _, item := range t.items {
		emoji := "○"
		switch item.Status {
		case "in_progress":
			emoji = "◐"
		case "completed":
			emoji = "✓"
		case "cancelled":
			emoji = "✗"
		}
		result += fmt.Sprintf("  %s [%s] %s\n", emoji, item.ID, item.Content)
	}
	return result
}

func (t *TodoTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"todos": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string"},
						"content": {"type": "string"},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]}
					},
					"required": ["id", "content", "status"]
				},
				"description": "List of todo items (replaces entire list unless merge=true)"
			},
			"merge": {
				"type": "boolean",
				"description": "If true, update/append items instead of replacing the entire list"
			}
		}
	}`
}
