package memory

// embed_check.go — embedding quality gate (local-first upgrade §9 risk item).
//
// The default embedder is the loaded local *chat* model's pooled embeddings
// via llama-server's --embedding endpoint. Some chat models produce usable
// sentence embeddings this way; others produce near-degenerate vectors that
// would make cosine recall WORSE than the keyword fallback. Since a bad
// embedder fails silently (recall just gets noisier), the wiring layer must
// not trust one blindly: ValidateEmbedder runs a small built-in probe suite
// and only passes when the model's vectors actually separate known-similar
// from known-dissimilar text by a margin. On failure the caller leaves the
// embedder unset and recall keeps its proven keyword behavior — the fallback
// guarantees vectors can only ever ADD recall quality, never lose it.

import (
	"context"
	"fmt"

	"github.com/darkcode/core"
)

// embedProbe is one labeled pair in the validation suite.
type embedProbe struct {
	a, b    string
	similar bool
}

// embedProbes are dev-domain sentence pairs with unambiguous labels. Similar
// pairs share meaning but deliberately share almost no tokens, so passing
// requires real semantic signal (keyword overlap couldn't fake it).
var embedProbes = []embedProbe{
	{a: "fix the login authentication bug", b: "repair the broken user sign-in flow", similar: true},
	{a: "read a file from disk", b: "load the document contents from storage", similar: true},
	{a: "deploy the service to production", b: "ship the release to the live environment", similar: true},
	{a: "fix the login authentication bug", b: "bake a chocolate cake for the party", similar: false},
	{a: "read a file from disk", b: "the weather in paris is sunny today", similar: false},
	{a: "deploy the service to production", b: "play a slow guitar solo on stage", similar: false},
}

// embedValidationMargin is the minimum required gap between the mean cosine
// of similar pairs and the mean cosine of dissimilar pairs. Degenerate
// embeddings (all vectors nearly identical) score ~0 and fail.
const embedValidationMargin = 0.10

// ValidateEmbedder embeds the probe suite with client and checks that cosine
// similarity separates similar from dissimilar pairs by at least
// embedValidationMargin. Returns nil when the embedder is trustworthy enough
// to wire into the memory system, or a diagnostic error describing why not
// (endpoint failure, degenerate vectors, insufficient margin).
func ValidateEmbedder(ctx context.Context, client core.LLMClient) error {
	if client == nil {
		return fmt.Errorf("nil embedder client")
	}

	// Embed every unique probe text once.
	vecs := make(map[string][]float32)
	dim := 0
	for _, p := range embedProbes {
		for _, text := range []string{p.a, p.b} {
			if _, ok := vecs[text]; ok {
				continue
			}
			v, err := client.CreateEmbedding(ctx, text)
			if err != nil {
				return fmt.Errorf("probe embedding failed: %w", err)
			}
			if len(v) == 0 {
				return fmt.Errorf("probe embedding is empty")
			}
			if dim == 0 {
				dim = len(v)
			} else if len(v) != dim {
				return fmt.Errorf("inconsistent embedding dimensions (%d vs %d)", len(v), dim)
			}
			vecs[text] = v
		}
	}

	var simSum, disSum float64
	var simN, disN int
	for _, p := range embedProbes {
		cos := cosineSimilarity(vecs[p.a], vecs[p.b])
		if p.similar {
			simSum += cos
			simN++
		} else {
			disSum += cos
			disN++
		}
	}
	simMean := simSum / float64(simN)
	disMean := disSum / float64(disN)
	margin := simMean - disMean
	if margin < embedValidationMargin {
		return fmt.Errorf("embeddings do not separate similar from dissimilar text (similar mean %.3f, dissimilar mean %.3f, margin %.3f < required %.2f) — likely degenerate pooled embeddings from a chat model",
			simMean, disMean, margin, embedValidationMargin)
	}
	return nil
}
