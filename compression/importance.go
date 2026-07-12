package compression

import (
	"math"
	"strings"
	"time"

	"github.com/darkcode/core"
)

// ============================================================================
// IMPORTANCE SCORING — Scores each message to determine which should be
// pinned (never compressed away) vs. compressible. This is the foundation
// of the hierarchical compression system: high-importance messages survive
// compression and are included verbatim in the compressed block.
//
// All scoring is heuristic (no LLM call needed). The scorer examines:
//   - Tool usage (messages with tool calls are action-bearing)
//   - Error content (error messages are critical context)
//   - User intent (user messages with questions/commands)
//   - File references (messages mentioning file paths)
//   - Recency (exponential decay by age)
// ============================================================================

// ImportanceScore holds the per-dimension scores for a single message.
type ImportanceScore struct {
	ToolUsage    float64 // messages with tool calls score higher
	ErrorContent float64 // error messages are critical context
	UserIntent   float64 // user messages with questions/commands
	FileRefs     float64 // messages mentioning file paths
	Recency      float64 // exponential decay by age
	Total        float64 // weighted sum of all dimensions
}

// ImportanceThreshold is the minimum score for a message to be "pinned"
// (kept verbatim during compression). Messages below this are compressible.
const ImportanceThreshold = 0.55

// Weights for each scoring dimension.
const (
	weightToolUsage    = 0.30
	weightErrorContent = 0.25
	weightUserIntent   = 0.20
	weightFileRefs     = 0.15
	weightRecency      = 0.10
)

// ScoreMessage computes the importance of a single message in the context
// of the conversation. Returns the composite score and the individual
// dimension scores for debugging/display.
func ScoreMessage(msg core.Message, msgIndex, totalMessages int, now time.Time) ImportanceScore {
	score := ImportanceScore{}

	// 1. Tool usage: messages with tool calls or tool results are action-bearing
	score.ToolUsage = scoreToolUsage(msg)

	// 2. Error content: error messages are critical context
	score.ErrorContent = scoreErrorContent(msg)

	// 3. User intent: user messages with questions/commands/decisions
	score.UserIntent = scoreUserIntent(msg)

	// 4. File references: messages mentioning file paths
	score.FileRefs = scoreFileRefs(msg)

	// 5. Recency: newer messages are more important (exponential decay)
	score.Recency = scoreRecency(msgIndex, totalMessages)

	// Compute weighted total
	score.Total = score.ToolUsage*weightToolUsage +
		score.ErrorContent*weightErrorContent +
		score.UserIntent*weightUserIntent +
		score.FileRefs*weightFileRefs +
		score.Recency*weightRecency

	return score
}

// ScoreMessages scores all messages and returns which should be pinned.
func ScoreMessages(messages []core.Message) (scores []ImportanceScore, pinned []int) {
	now := time.Now()
	total := len(messages)
	scores = make([]ImportanceScore, total)

	for i, msg := range messages {
		scores[i] = ScoreMessage(msg, i, total, now)
		if scores[i].Total >= ImportanceThreshold {
			pinned = append(pinned, i)
		}
	}
	return scores, pinned
}

// IsPinned returns whether a message at the given index is important enough
// to keep verbatim during compression.
func IsPinned(msg core.Message, msgIndex, totalMessages int) bool {
	score := ScoreMessage(msg, msgIndex, totalMessages, time.Now())
	return score.Total >= ImportanceThreshold
}

// ---- dimension scorers ----

// scoreToolUsage returns 0.0-1.0 based on tool call presence.
func scoreToolUsage(msg core.Message) float64 {
	// Tool result messages are always important (they carry action outcomes)
	if msg.Role == core.RoleTool {
		return 0.9
	}
	// Assistant messages with tool calls carry actions
	if len(msg.ToolCalls) > 0 {
		// More tool calls = more important (capped at 1.0)
		n := float64(len(msg.ToolCalls))
		return math.Min(0.7+n*0.1, 1.0)
	}
	return 0.0
}

// scoreErrorContent returns 0.0-1.0 based on error indicators.
func scoreErrorContent(msg core.Message) float64 {
	content := strings.ToLower(msg.ContentString())
	if content == "" {
		return 0.0
	}

	score := 0.0

	// Strong error indicators
	strongIndicators := []string{
		"error:", "error :", "fatal:", "panic:",
		"failed:", "failure:", "exception:",
		"traceback", "stack trace", "segfault",
		"permission denied", "not found", "does not exist",
		"cannot ", "couldn't ", "unable to ",
	}
	for _, ind := range strongIndicators {
		if strings.Contains(content, ind) {
			score = math.Max(score, 0.9)
			break
		}
	}

	// Weaker error indicators
	weakIndicators := []string{
		"warning:", "warn:", "deprecated",
		"timeout", "refused", "rejected",
	}
	for _, ind := range weakIndicators {
		if strings.Contains(content, ind) {
			score = math.Max(score, 0.6)
			break
		}
	}

	return score
}

// scoreUserIntent returns 0.0-1.0 based on user message characteristics.
func scoreUserIntent(msg core.Message) float64 {
	if msg.Role != core.RoleUser {
		return 0.0
	}

	content := strings.ToLower(msg.ContentString())
	if content == "" {
		return 0.1 // empty user message has low intent
	}

	score := 0.3 // base score for any user message

	// Questions (high intent — the user expects an answer)
	if strings.ContainsAny(content, "?") {
		score = math.Max(score, 0.7)
	}

	// Commands / directives
	commandPrefixes := []string{
		"create ", "build ", "write ", "implement ", "fix ",
		"delete ", "remove ", "update ", "modify ", "change ",
		"run ", "execute ", "deploy ", "test ", "debug ",
		"install ", "configure ", "setup ", "set up ",
		"please ", "can you ", "i want ", "i need ",
	}
	for _, p := range commandPrefixes {
		if strings.HasPrefix(content, p) || strings.Contains(content, " "+p) {
			score = math.Max(score, 0.8)
			break
		}
	}

	// Decisions / constraints
	decisionIndicators := []string{
		"use ", "prefer ", "don't ", "do not ", "must ",
		"should ", "always ", "never ", "instead ",
	}
	for _, d := range decisionIndicators {
		if strings.Contains(content, d) {
			score = math.Max(score, 0.6)
			break
		}
	}

	// Short messages (like "ok", "yes", "no") are low importance
	if len(content) < 10 {
		score = math.Min(score, 0.2)
	}

	return score
}

// scoreFileRefs returns 0.0-1.0 based on file path mentions.
func scoreFileRefs(msg core.Message) float64 {
	content := msg.ContentString()
	if content == "" {
		return 0.0
	}

	pathCount := 0
	for _, tok := range strings.Fields(content) {
		tok = strings.Trim(tok, "\"'`,.;()[]{}:")
		if len(tok) < 3 || len(tok) > 256 {
			continue
		}
		// Absolute paths or paths with a slash and an extension
		if (strings.HasPrefix(tok, "/") || strings.Contains(tok, "/")) && strings.Contains(tok, ".") {
			pathCount++
		}
	}

	if pathCount == 0 {
		return 0.0
	}
	// Diminishing returns: 1 path = 0.5, 2 = 0.7, 3+ = 0.9
	return math.Min(0.3+float64(pathCount)*0.2, 0.9)
}

// scoreRecency returns 0.0-1.0 with newer messages scoring higher.
// Uses an exponential curve: the last 20% of messages score > 0.8.
func scoreRecency(msgIndex, totalMessages int) float64 {
	if totalMessages <= 1 {
		return 1.0
	}
	// Position as fraction [0, 1] where 1 = most recent
	pos := float64(msgIndex) / float64(totalMessages-1)
	// Exponential curve: e^(3*(pos-1)) gives nice decay
	return math.Exp(3 * (pos - 1))
}
