package router

// confidence.go — ConfidenceScorer evaluates how confident a model's response
// appears. Previously this returned a hardcoded 0.9 for any response that
// didn't contain a hedge phrase. It now performs a multi-signal analysis:
//
//   - Hedge phrases ("I'm not sure", "might be", "approximately") → penalty
//   - Confidence markers ("definitely", "the answer is", "certainly") → boost
//   - Question marks in the response (uncertainty) → penalty
//   - Code blocks / structured output (decisiveness) → boost
//   - Response length: very short or very long → slight penalty
//   - Refusal markers ("I can't", "I cannot") → low confidence
//
// The final score is clamped to [0.0, 1.0].

import (
	"math"
	"strings"
)

// ConfidenceScorer evaluates the confidence of a model's response.
type ConfidenceScorer struct{}

func NewConfidenceScorer() *ConfidenceScorer {
	return &ConfidenceScorer{}
}

// Score gives a score between 0.0 and 1.0 based on response characteristics.
func (s *ConfidenceScorer) Score(response string) float64 {
	if strings.TrimSpace(response) == "" {
		return 0.0
	}
	lower := strings.ToLower(response)
	score := 0.7 // neutral baseline

	// --- Refusal / inability markers → strong penalty ---
	refusals := []string{"i can't", "i cannot", "i'm unable", "i am unable", "as an ai", "i don't have access"}
	for _, p := range refusals {
		if strings.Contains(lower, p) {
			score -= 0.4
		}
	}

	// --- Hedge phrases → penalty ---
	hedges := []string{
		"i'm not sure", "i am not sure", "i think", "might be", "could be",
		"maybe", "perhaps", "possibly", "approximately", "roughly",
		"i believe", "it seems", "i would guess", "not certain",
	}
	hedgeCount := 0
	for _, p := range hedges {
		if strings.Contains(lower, p) {
			hedgeCount++
		}
	}
	score -= float64(hedgeCount) * 0.15

	// --- Confidence markers → boost ---
	confidenceMarkers := []string{
		"definitely", "certainly", "the answer is", "exactly", "precisely",
		"the result is", "here is", "here's the", "the solution is",
	}
	for _, p := range confidenceMarkers {
		if strings.Contains(lower, p) {
			score += 0.1
		}
	}

	// --- Question marks in response (uncertainty) → penalty ---
	qCount := strings.Count(response, "?")
	if qCount > 0 {
		score -= math.Min(float64(qCount)*0.05, 0.2)
	}

	// --- Code blocks / structured output → boost ---
	codeBlocks := strings.Count(response, "```")
	if codeBlocks > 0 {
		score += 0.1
	}
	// Bullet points or numbered lists → structured → boost
	if strings.Contains(response, "\n- ") || strings.Contains(response, "\n* ") ||
		strings.Contains(response, "\n1. ") || strings.Contains(response, "\n1) ") {
		score += 0.05
	}

	// --- Response length penalties ---
	lenResp := len(response)
	switch {
	case lenResp < 20:
		score -= 0.1 // too terse to be confident
	case lenResp > 5000:
		score -= 0.05 // rambling
	}

	// Clamp to [0.0, 1.0].
	if score < 0.0 {
		score = 0.0
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}
