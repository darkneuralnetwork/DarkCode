package orchestrator

import (
	"fmt"
	"strings"
	"github.com/darkcode/core"
)

// ErrorManager handles LLM and system errors, providing auto-fixes for known
// strict schema validations like Gemini's thought_signature constraint.
type ErrorManager struct{}

func NewErrorManager() *ErrorManager {
	return &ErrorManager{}
}

// Handle inspects the error and the message history.
// If the error is an API 400 schema error (like thought_signature missing),
// it rewrites the history by converting past tool calls into plain text
// to bypass the validation while preserving semantic context.
// Returns true if the history was modified and the call should be retried.
func (em *ErrorManager) Handle(err error, history []core.Message) (bool, []core.Message) {
	if err == nil {
		return false, history
	}

	errStr := err.Error()

	// Gemini API "thought_signature" or INVALID_ARGUMENT 400 error auto-fix
	// "Function call is missing a thought_signature in functionCall parts"
	if strings.Contains(errStr, "thought_signature") || strings.Contains(errStr, "INVALID_ARGUMENT") {
		modified := false
		var newHistory []core.Message

		for _, m := range history {
			newMsg := m
			
			// 1. Convert Assistant ToolCalls to plain text
			if len(m.ToolCalls) > 0 {
				var calls []string
				for _, tc := range m.ToolCalls {
					calls = append(calls, fmt.Sprintf("%s(%s)", tc.Function.Name, tc.Function.Arguments))
				}
				callStr := strings.Join(calls, ", ")
				
				newMsg.ToolCalls = nil
				if newMsg.ContentString() == "" {
					newMsg.Content = fmt.Sprintf("[Historical Tool Calls Executed: %s]", callStr)
				} else {
					newMsg.Content = newMsg.ContentString() + fmt.Sprintf("\n[Historical Tool Calls Executed: %s]", callStr)
				}
				modified = true
			}
			
			// 2. Convert Tool Responses to plain User/Assistant text
			if m.Role == core.RoleTool {
				newMsg.Role = core.RoleUser
				newMsg.Content = fmt.Sprintf("Tool Result for %s: %s", m.ToolCallID, newMsg.ContentString())
				newMsg.ToolCallID = ""
				modified = true
			}

			newHistory = append(newHistory, newMsg)
		}

		return modified, newHistory
	}

	return false, history
}
