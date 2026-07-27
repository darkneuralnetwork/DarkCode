package server

// openai_compat.go — an OpenAI-compatible surface so DarkCode can be pointed
// at by any client that already speaks that wire format (Open WebUI,
// LibreChat, editor plugins, scripts using the openai SDK with a custom
// base_url). The agent is exposed as a single model; a request's messages are
// flattened to the latest user turn and run through the same kernel the web
// chat uses, so behaviour, memory and permissions are identical.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
)

// compatModelID is the model name reported to OpenAI-compatible clients.
const compatModelID = "darkcode"

// handleOpenAIModels implements GET /v1/models.
func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{{
			"id": compatModelID, "object": "model",
			"created": time.Now().Unix(), "owned_by": "darkcode",
		}},
	})
}

// handleOpenAIChat implements POST /v1/chat/completions.
func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// The kernel owns conversation state, so only the newest user turn is
	// taken from the request; earlier turns would double-count history.
	prompt := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = compatContentText(req.Messages[i].Content)
			break
		}
	}
	if strings.TrimSpace(prompt) == "" {
		writeError(w, http.StatusBadRequest, "no user message found")
		return
	}

	metrics.Default.RecordTurn()
	ctx := context.WithValue(r.Context(), core.WorkspaceKey, s.ActiveWorkspace())
	answer, err := s.kernel.Execute(ctx, prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.Stream {
		s.streamOpenAIChat(w, answer)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   compatModelID,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
	})
}

// streamOpenAIChat emits the answer as a single SSE delta followed by [DONE].
// The kernel returns a complete answer rather than a token stream, so chunking
// it further would fake a progressiveness that isn't there; clients that ask
// for a stream still get a well-formed one.
func (s *Server) streamOpenAIChat(w http.ResponseWriter, answer string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
	chunk := func(delta map[string]interface{}, finish interface{}) {
		payload, _ := json.Marshal(map[string]interface{}{
			"id": id, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": compatModelID,
			"choices": []map[string]interface{}{{
				"index": 0, "delta": delta, "finish_reason": finish,
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}
	chunk(map[string]interface{}{"role": "assistant", "content": answer}, nil)
	chunk(map[string]interface{}{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// compatContentText accepts both content shapes the OpenAI API allows: a plain
// string, or an array of typed parts.
func compatContentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
