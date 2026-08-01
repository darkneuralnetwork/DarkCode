package llm

// window.go — how much context a model actually has.
//
// ModelInfo used to answer this with a guess: 8,000 tokens for everything,
// bumped to 32,000 if the model name happened to contain "32k" or "claude-3".
// Every current cloud model got 8,000. Gemini 2.5 Flash has 1,048,576, so the
// figure the compression trigger was working from was out by a factor of 131,
// and the one model the heuristic did recognise — Claude 3.5 — got 32,000
// against a real window of 200,000.
//
// Under-reporting is safe but expensive: it fires compression long before the
// window is anywhere near full, spending a model call and discarding context
// that would have fit. Over-reporting is the dangerous direction — the request
// is rejected outright — so unknown models return 0 ("I don't know") and the
// caller falls back to the configured value, rather than getting a number this
// file invented.
//
// The provider catalogue in config/providers.go already carries this figure per
// model, curated alongside the pricing, so that is consulted first — an exact
// id from a maintained list beats any pattern. The table below is the fallback
// for what the catalogue does not list: self-hosted builds, models released
// since the catalogue was last updated, and the dated or prefixed ids
// (`claude-haiku-4-5-20251001`, `openai/gpt-4o`) that an exact-match lookup
// misses.

import (
	"strings"

	"github.com/darkcode/config"
)

// window pairs a model-id fragment with the context window of every model whose
// id contains it. Order matters: the first match wins, so narrower fragments
// have to come before the families that contain them.
type window struct {
	match  string
	tokens int
}

// Conservative by design. Where a family's members differ, the smaller figure
// is used: being early to compress costs a call, being late costs the request.
var windows = []window{
	// --- Anthropic ---
	// The 1M window on Sonnet 4+ is beta and header-gated, so the number here
	// is the one available without opting in.
	{"claude-haiku-4", 200000},
	{"claude-sonnet-4", 200000},
	{"claude-opus-4", 200000},
	{"claude-sonnet-5", 200000},
	{"claude-haiku-5", 200000},
	{"claude-opus-5", 200000},
	{"claude-fable-5", 200000},
	{"claude-3-7", 200000},
	{"claude-3-5", 200000},
	{"claude-3", 200000},
	{"claude", 200000},

	// --- Google ---
	{"gemini-2.5-pro", 1048576},
	{"gemini-2.5-flash", 1048576},
	{"gemini-2.0-flash", 1048576},
	{"gemini-1.5-pro", 2097152},
	{"gemini-1.5-flash", 1048576},
	{"gemini", 1048576},

	// --- OpenAI ---
	{"gpt-4o-mini", 128000},
	{"gpt-4o", 128000},
	{"gpt-4.1", 1047576},
	{"gpt-4-turbo", 128000},
	{"gpt-4-32k", 32768},
	{"gpt-4", 8192},
	{"gpt-3.5-turbo-16k", 16385},
	{"gpt-3.5", 16385},
	{"o4-mini", 200000},
	{"o3-mini", 200000},
	{"o3", 200000},
	{"o1-mini", 128000},
	{"o1", 200000},

	// --- Open-weight families, commonly served via OpenAI-compatible APIs ---
	{"deepseek-r1", 65536},
	{"deepseek", 65536},
	{"qwen3", 131072},
	{"qwen2.5", 131072},
	{"qwen2_5", 131072},
	{"qwen", 32768},
	{"llama-3.3", 131072},
	{"llama-3.2", 131072},
	{"llama-3.1", 131072},
	{"llama-3", 8192},
	{"mixtral", 32768},
	{"mistral-large", 131072},
	{"mistral", 32768},
	{"command-r-plus", 131072},
	{"command-r", 131072},
	{"grok", 131072},
	{"phi-4", 16384},
	{"phi-3", 131072},
	{"gemma-3", 131072},
	{"gemma-2", 8192},
	{"gemma", 8192},
}

// ContextWindowFor returns the context window in tokens for a model id, or 0
// when the model is not recognised.
//
// Zero means "unknown", not "none". Callers fall back to the configured value,
// which is a number a human chose rather than one this table guessed.
func ContextWindowFor(model string) int {
	name := strings.TrimSpace(model)
	if name == "" {
		return 0
	}
	// The curated catalogue first: an exact id from a maintained list is better
	// evidence than a pattern match.
	if w := config.CatalogContextWindow(name); w > 0 {
		return w
	}

	m := strings.ToLower(name)
	// A provider prefix ("openai/gpt-4o", "google/gemini-2.5-flash") is common
	// on aggregators and must not defeat the match.
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		m = m[i+1:]
	}
	for _, w := range windows {
		if strings.Contains(m, w.match) {
			return w.tokens
		}
	}
	// An explicit size in the name is better evidence than anything this table
	// knows, and it is how self-hosted builds usually advertise the window.
	if n := windowFromName(m); n > 0 {
		return n
	}
	return 0
}

// windowFromName reads a "…-128k…" or "…-32k…" marker out of a model id.
func windowFromName(m string) int {
	for i := 0; i < len(m); i++ {
		if m[i] != 'k' && m[i] != 'K' {
			continue
		}
		// Walk back over the digits immediately before the k.
		j := i
		for j > 0 && m[j-1] >= '0' && m[j-1] <= '9' {
			j--
		}
		if j == i {
			continue // a bare "k", not a size
		}
		// Must be a whole token, not the tail of a word like "tokens".
		if i+1 < len(m) && (isAlnum(m[i+1])) {
			continue
		}
		n := 0
		for _, c := range m[j:i] {
			n = n*10 + int(c-'0')
		}
		if n > 0 && n <= 10000 { // sanity: "1000k" is plausible, "99999k" is not
			return n * 1024
		}
	}
	return 0
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
