package core

// tokens.go — the one token estimator.
//
// Four packages used to answer "how many tokens is this?" four different ways:
// llm counted runes by script, compression counted whitespace-separated words,
// ctxengine and memory divided bytes by four. On English prose they agreed, so
// the divergence stayed invisible; on Go source the word-based estimate ran
// about 29% low, because code has few spaces relative to its length.
//
// Under-counting is the dangerous direction. Every caller here is deciding how
// much history fits in a context window, so an estimate that reads low packs
// too much in and the provider rejects the request — the failure lands at the
// API boundary, far from the arithmetic that caused it. One estimator means a
// budget computed in one package still means the same thing in the next.
//
// This is a heuristic and cannot be otherwise: the real count depends on the
// model's tokenizer. Callers that need exactness should use provider-reported
// usage, which is why this is only consulted when that is unavailable.

// EstimateTokens approximates the token count of s.
//
// It is rune-aware rather than byte-based so CJK text is not over-counted:
// those scripts tokenize at roughly 1.5 characters per token for typical BPE
// vocabularies, while a byte-length heuristic would charge three bytes each.
// ASCII keeps the classic ~4 characters per token, so the common case is
// unchanged.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var ascii, cjk, other int
	for _, r := range s {
		switch {
		case r < 0x80:
			ascii++
		case r >= 0x3000 && r <= 0x30FF, // CJK symbols + Japanese kana
			r >= 0x3400 && r <= 0x4DBF, // CJK Extension A
			r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
			r >= 0xAC00 && r <= 0xD7AF, // Hangul Syllables
			r >= 0xF900 && r <= 0xFAFF, // CJK Compatibility Ideographs
			r >= 0xFF00 && r <= 0xFFEF: // Halfwidth/Fullwidth Forms
			cjk++
		default:
			other++
		}
	}
	// Any non-empty string costs at least one token.
	if tokens := ascii/4 + cjk*2/3 + other/2; tokens > 0 {
		return tokens
	}
	return 1
}
