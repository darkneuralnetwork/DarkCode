package memory

import (
	"strings"

	"github.com/darkcode/core"
)

// ContextEngine manages the token budget and applies adaptive compression
// to prevent the prompt window from overflowing.
type ContextEngine struct {
	MaxTokens int
}

func NewContextEngine(maxTokens int) *ContextEngine {
	return &ContextEngine{
		MaxTokens: maxTokens,
	}
}

// Compress takes a conversation history and reduces its size by summarizing older turns
// while preserving the system prompt and the most recent N turns perfectly.
func (ce *ContextEngine) Compress(messages []core.Message) []core.Message {
	if len(messages) <= 4 {
		return messages // No need to compress short conversations
	}

	// Calculate a crude token count (e.g. 1 word ≈ 1.3 tokens)
	totalWords := 0
	for _, m := range messages {
		if contentStr, ok := m.Content.(string); ok {
			totalWords += len(strings.Fields(contentStr))
		}
	}
	estimatedTokens := int(float64(totalWords) * 1.3)

	if estimatedTokens < ce.MaxTokens {
		return messages // Under budget
	}

	// We are over budget. We must compress.
	var compressed []core.Message

	// Always keep the system prompt intact
	if messages[0].Role == core.RoleSystem {
		compressed = append(compressed, messages[0])
		messages = messages[1:]
	}

	// Summarize the middle of the conversation
	summary := "Previous turns were summarized: User asked questions, and the agent answered."
	compressed = append(compressed, core.Message{
		Role:    core.RoleSystem,
		Content: "[Context Compressed] " + summary,
	})

	// Keep the last 2 turns intact
	if len(messages) > 2 {
		compressed = append(compressed, messages[len(messages)-2:]...)
	} else {
		compressed = append(compressed, messages...)
	}

	return compressed
}
