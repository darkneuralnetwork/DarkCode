package loop

import (
	"strings"
	"testing"

	"github.com/darkcode/tools"
)

func TestSystemPromptIncludesRepoRulesWhenSet(t *testing.T) {
	l := New(newTestRouter(&fakeLLMClient{}), tools.NewRegistry(), nil, 1)
	l.SetRepoRules("never commit directly to main")

	got := l.systemPrompt()
	if !strings.Contains(got, "never commit directly to main") {
		t.Error("system prompt is missing the repo rules content")
	}
}

func TestSystemPromptOmitsRepoRulesSectionWhenEmpty(t *testing.T) {
	l := New(newTestRouter(&fakeLLMClient{}), tools.NewRegistry(), nil, 1)

	got := l.systemPrompt()
	if strings.Contains(got, "Project Rules") {
		t.Error("system prompt should omit the Project Rules section when no rules are configured")
	}
}
