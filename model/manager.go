package model

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// ModelMetadata represents a model available for download.
type ModelMetadata struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	SizeMB    int    `json:"size_mb"`
	Checksum  string `json:"checksum"`
	Format    string `json:"format"`
}

// Manager is responsible for discovering, downloading, and storing AI models.
type Manager struct {
	modelDir string
	models   map[string]ModelMetadata
}

func NewManager(modelDir string) *Manager {
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		fmt.Printf("Failed to create model dir: %v\n", err)
	}
	return &Manager{
		modelDir: modelDir,
		models:   make(map[string]ModelMetadata),
	}
}

// Recommend suggests a model based on RAM size.
func (m *Manager) Recommend(ramMB float64) ModelMetadata {
	if ramMB > 24000 {
		return ModelMetadata{ID: "llama-3-8b", Name: "Llama 3 (8B) GGUF", URL: "https://huggingface.co/...", SizeMB: 4800, Format: "gguf"}
	} else if ramMB > 8000 {
		return ModelMetadata{ID: "phi-3-mini", Name: "Phi-3 Mini (3.8B) GGUF", URL: "https://huggingface.co/...", SizeMB: 2300, Format: "gguf"}
	}
	return ModelMetadata{ID: "qwen-1.5b", Name: "Qwen 1.5B GGUF", URL: "https://huggingface.co/...", SizeMB: 900, Format: "gguf"}
}

// Download fetches a model if it does not already exist.
func (m *Manager) Download(model ModelMetadata) error {
	dest := filepath.Join(m.modelDir, model.ID+".gguf")
	if _, err := os.Stat(dest); err == nil {
		// Already exists
		return nil
	}

	// In a complete implementation, this would stream the download, show a progress bar,
	// and verify the sha256 checksum after downloading.
	resp, err := http.Get(model.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// GetModelPath returns the path to a downloaded model.
func (m *Manager) GetModelPath(modelID string) string {
	return filepath.Join(m.modelDir, modelID+".gguf")
}
