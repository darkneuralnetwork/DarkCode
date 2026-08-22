package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry represents a single memory entry.
type Entry struct {
	ID        string    `json:"id"`
	Category  string    `json:"category"` // "user" or "memory"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is a persistent JSON-backed memory store. Thread-safe.
type Store struct {
	mu      sync.RWMutex
	path    string
	entries []Entry
}

// NewStore creates or loads a memory store from the given path.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// DefaultStorePath returns the default memory file path.
func DefaultStorePath() string {
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".darkcode", "memory.json")
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.entries = make([]Entry, 0)
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.entries)
}

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Add creates a new memory entry.
func (s *Store) Add(category, content string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := Entry{
		ID:        fmt.Sprintf("mem_%d", len(s.entries)+1),
		Category:  category,
		Content:   content,
		CreatedAt: time.Now(),
	}
	s.entries = append(s.entries, entry)

	if err := s.save(); err != nil {
		return nil, err
	}
	return &entry, nil
}

// Replace updates an existing memory entry identified by a substring match.
func (s *Store) Replace(oldText, newContent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, entry := range s.entries {
		if strings.Contains(entry.Content, oldText) {
			s.entries[i].Content = newContent
			return s.save()
		}
	}
	return fmt.Errorf("no memory entry found containing: %s", oldText)
}

// Remove deletes a memory entry identified by a substring match.
func (s *Store) Remove(oldText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, entry := range s.entries {
		if strings.Contains(entry.Content, oldText) {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("no memory entry found containing: %s", oldText)
}

// List returns all memory entries, optionally filtered by category.
func (s *Store) List(category string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Entry
	for _, entry := range s.entries {
		if category == "" || entry.Category == category {
			result = append(result, entry)
		}
	}
	return result
}

// Search returns entries containing the query string.
func (s *Store) Search(query string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query = strings.ToLower(query)
	var result []Entry
	for _, entry := range s.entries {
		if strings.Contains(strings.ToLower(entry.Content), query) {
			result = append(result, entry)
		}
	}
	return result
}

// FormatEntries formats memory entries as a readable string.
func FormatEntries(entries []Entry) string {
	if len(entries) == 0 {
		return "No memory entries found."
	}

	// Sort by created_at descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", e.Category, e.ID, e.Content))
	}
	return sb.String()
}

// AsContext returns all memory entries formatted for the system prompt.
func (s *Store) AsContext() string {
	entries := s.List("")
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n## Persistent Memory\n")
	sb.WriteString(FormatEntries(entries))
	return sb.String()
}
