package llm

import (
	"testing"
	"time"

	"github.com/darkcode/core"
)

func systemAndUser() []core.Message {
	return []core.Message{
		{Role: core.RoleSystem, Content: "You are a coding agent."},
		{Role: core.RoleUser, Content: "hi"},
	}
}

// Anthropic and OpenRouter cache only what carries an explicit breakpoint.
func TestOutgoingMarksSystemForExplicitCacheProviders(t *testing.T) {
	for _, prov := range []string{"anthropic", "openrouter"} {
		c := &Client{Provider: prov}
		out := c.outgoing(&core.CompletionRequest{Messages: systemAndUser()})

		parts, ok := out.Messages[0].Content.([]core.ContentPart)
		if !ok {
			t.Fatalf("%s: system content = %T, want []ContentPart", prov, out.Messages[0].Content)
		}
		if len(parts) != 1 || parts[0].CacheControl == nil || parts[0].CacheControl.Type != "ephemeral" {
			t.Errorf("%s: missing ephemeral breakpoint: %+v", prov, parts)
		}
		if parts[0].Text != "You are a coding agent." {
			t.Errorf("%s: prompt text was altered: %q", prov, parts[0].Text)
		}
		if out.Messages[1].Content != "hi" {
			t.Errorf("%s: non-system message was rewritten", prov)
		}
	}
}

// OpenAI caches automatically; sending content parts there is needless churn.
func TestOutgoingLeavesOtherProvidersAlone(t *testing.T) {
	for _, prov := range []string{"openai", "ollama", "lmstudio", ""} {
		c := &Client{Provider: prov}
		out := c.outgoing(&core.CompletionRequest{Messages: systemAndUser()})
		if _, isString := out.Messages[0].Content.(string); !isString {
			t.Errorf("%s: system message should stay a plain string, got %T", prov, out.Messages[0].Content)
		}
	}
}

// Consensus fans one request out to several providers, so marking must not
// mutate the caller's messages.
func TestOutgoingDoesNotMutateCaller(t *testing.T) {
	msgs := systemAndUser()
	req := &core.CompletionRequest{Messages: msgs}

	(&Client{Provider: "anthropic"}).outgoing(req)

	if _, isString := msgs[0].Content.(string); !isString {
		t.Fatalf("caller's message was mutated: %T", msgs[0].Content)
	}
	if _, isString := req.Messages[0].Content.(string); !isString {
		t.Fatalf("caller's request was mutated: %T", req.Messages[0].Content)
	}
}

func TestOutgoingAppliesEffortWithoutOverridingRequest(t *testing.T) {
	c := &Client{Provider: "openai", Effort: "high"}

	if got := c.outgoing(&core.CompletionRequest{Messages: systemAndUser()}).ReasoningEffort; got != "high" {
		t.Errorf("client default effort not applied: %q", got)
	}
	req := &core.CompletionRequest{Messages: systemAndUser(), ReasoningEffort: "low"}
	if got := c.outgoing(req).ReasoningEffort; got != "low" {
		t.Errorf("per-request effort was overridden: %q", got)
	}
	if got := (&Client{Provider: "openai"}).outgoing(req).ReasoningEffort; got != "low" {
		t.Errorf("effort should pass through unset clients: %q", got)
	}
}

// A request with no system message must survive marking untouched.
func TestOutgoingHandlesMissingSystemMessage(t *testing.T) {
	c := &Client{Provider: "anthropic"}
	msgs := []core.Message{{Role: core.RoleUser, Content: "hi"}}
	out := c.outgoing(&core.CompletionRequest{Messages: msgs})
	if len(out.Messages) != 1 || out.Messages[0].Content != "hi" {
		t.Errorf("messages altered: %+v", out.Messages)
	}
}

func TestKeyPoolRotatesAndParksThrottledKeys(t *testing.T) {
	p := NewKeyPool("a", "b", "c")
	if p.Len() != 3 {
		t.Fatalf("Len = %d, want 3", p.Len())
	}

	// Round-robin across all keys.
	seen := map[string]bool{p.Get(): true, p.Get(): true, p.Get(): true}
	if len(seen) != 3 {
		t.Errorf("expected all 3 keys in rotation, saw %v", seen)
	}

	p.Penalize("a", time.Hour)
	p.Penalize("b", time.Hour)
	for i := 0; i < 5; i++ {
		if got := p.Get(); got != "c" {
			t.Fatalf("got %q, want the only healthy key \"c\"", got)
		}
	}

	// With everything parked the pool still returns something to send.
	p.Penalize("c", time.Hour)
	if got := p.Get(); got == "" {
		t.Error("pool returned no key when all were cooling down")
	}
}

func TestKeyPoolDropsBlanksAndDuplicates(t *testing.T) {
	p := NewKeyPool("k", "", "k", "j")
	if p.Len() != 2 {
		t.Errorf("Len = %d, want 2", p.Len())
	}
	if NewKeyPool("", "") != nil {
		t.Error("a pool with no usable key should be nil")
	}
}

// A nil pool is the single-key default and must be safe to call.
func TestNilKeyPoolIsInert(t *testing.T) {
	var p *KeyPool
	if p.Get() != "" || p.Len() != 0 {
		t.Error("nil pool should report empty")
	}
	p.Penalize("x", time.Hour) // must not panic

	c := &Client{APIKey: "single"}
	if got := c.pickKey(); got != "single" {
		t.Errorf("pickKey = %q, want the configured APIKey", got)
	}
}

func TestPenalizeOnlyParksCredentialFailures(t *testing.T) {
	c := &Client{Keys: NewKeyPool("a", "b")}

	c.penalize("a", &APIError{Code: 500})
	if got := c.Keys.Get(); got != "a" {
		t.Errorf("a 500 is not a credential problem; key should stay in rotation, got %q", got)
	}

	c.Keys = NewKeyPool("a", "b")
	c.penalize("a", &APIError{Code: 429})
	for i := 0; i < 3; i++ {
		if got := c.Keys.Get(); got != "b" {
			t.Fatalf("throttled key was not parked, got %q", got)
		}
	}
}
