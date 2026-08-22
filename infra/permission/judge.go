package permission

// judge.go — optional LLM-assisted approval.
//
// The deterministic classifier is intentionally broad: it flags `curl`, `tar`
// and `mv` because each *can* be destructive. In practice most flagged calls
// are routine, and a stream of prompts trains the user to approve without
// reading — which is worse than not prompting at all.
//
// A judge narrows that set. It may only ever turn a prompt into a silent
// approval for calls the classifier rated low or medium risk; critical and
// high-risk actions, deny-rule matches, and anything carrying a secret still
// go to the user. The judge can reduce noise, never raise privilege.

import (
	"strings"
	"sync"

	"github.com/darkcode/infra/core"
)

// Judge decides whether a flagged call is routine enough to skip the prompt.
// It returns true to auto-approve. Any error or uncertainty must return false
// so the user is asked — the safe direction is always "ask".
type Judge func(req ApprovalRequest) bool

// judgeState holds the optional judge, guarded separately so installing one
// never contends with the approval path's own lock.
type judgeState struct {
	mu sync.RWMutex
	fn Judge
}

// SetJudge installs (or clears, with nil) the auto-approval judge.
func (g *Gate) SetJudge(j Judge) {
	g.judge.mu.Lock()
	g.judge.fn = j
	g.judge.mu.Unlock()
}

// judgeAllows reports whether the judge clears this request without prompting.
// Risk is the ceiling: a judge cannot wave through a high or critical action,
// so a confused or compromised model cannot escalate anything.
func (g *Gate) judgeAllows(req ApprovalRequest) bool {
	if req.Risk == core.RiskHigh || req.Risk == core.RiskCritical {
		return false
	}
	g.judge.mu.RLock()
	fn := g.judge.fn
	g.judge.mu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(req)
}

// ParseJudgeVerdict reads a model's reply to an approval question. Only an
// unambiguous "safe" counts; anything else — including an explanation, a
// refusal, or noise — means ask the user.
func ParseJudgeVerdict(reply string) bool {
	r := strings.ToLower(strings.TrimSpace(reply))
	r = strings.Trim(r, " .\"'`*\n\t")
	return r == "safe" || strings.HasPrefix(r, "safe:") || strings.HasPrefix(r, "safe ")
}

// JudgePrompt builds the question put to the auxiliary model. It asks for one
// word, because a judge that writes prose is a judge whose output has to be
// parsed loosely — and loose parsing on a security decision fails open.
func JudgePrompt(req ApprovalRequest) string {
	var b strings.Builder
	b.WriteString("You are a security reviewer for a coding agent. Decide whether this action is routine ")
	b.WriteString("developer work that needs no human confirmation.\n\n")
	b.WriteString("Answer with exactly one word: SAFE or ASK.\n")
	b.WriteString("Answer ASK if the action deletes data, changes permissions, touches credentials or ")
	b.WriteString("system configuration, sends data off the machine, or you are unsure.\n\n")
	b.WriteString("Tool: " + req.Tool + "\n")
	if req.Summary != "" {
		b.WriteString("Summary: " + req.Summary + "\n")
	}
	if req.Preview != "" {
		preview := req.Preview
		if len(preview) > 1000 {
			preview = preview[:1000]
		}
		b.WriteString("Details:\n" + preview + "\n")
	}
	return b.String()
}
