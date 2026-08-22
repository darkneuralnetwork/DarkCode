package core

import "time"

// SemanticEntry is a knowledge entry in semantic memory.
type SemanticEntry struct {
	Key       string    `json:"key"`
	Content   string    `json:"content"`
	Category  string    `json:"category,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Vector    []float32 `json:"vector,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
