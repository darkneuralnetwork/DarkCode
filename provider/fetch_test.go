package provider

import (
	"reflect"
	"testing"
)

func TestSortGeminiModels(t *testing.T) {
	in := []string{
		"gemini-2.0-flash",
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-3-pro",
		"gemini-2.0-pro",
	}
	sortGeminiModels(in)
	want := []string{
		"gemini-3-pro",    // newest generation first
		"gemini-2.5-flash", // then 2.5, alphabetical within generation
		"gemini-2.5-pro",
		"gemini-2.0-flash",
		"gemini-2.0-pro",
	}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("sortGeminiModels =\n  %v\nwant\n  %v", in, want)
	}
}

func TestKeepGeminiModel(t *testing.T) {
	chat := []string{"generateContent", "countTokens"}
	cases := []struct {
		id      string
		methods []string
		keep    bool
	}{
		// Current chat models — keep.
		{"gemini-2.5-flash", chat, true},
		{"gemini-2.0-flash", chat, true},
		{"gemini-2.5-pro", chat, true},
		{"gemini-3-pro", chat, true}, // future generation
		// Deprecated generations — drop.
		{"gemini-1.5-flash", chat, false},
		{"gemini-1.5-pro-002", chat, false},
		{"gemini-1.0-pro", chat, false},
		{"gemini-pro", chat, false},        // legacy unversioned (1.0-era)
		{"gemini-pro-vision", chat, false}, // vision-only + legacy
		// Non-chat / specialized — drop even though some report generateContent.
		{"embedding-001", []string{"embedContent"}, false},
		{"text-embedding-004", []string{"embedContent"}, false},
		{"aqa", []string{"generateAnswer"}, false},
		{"imagen-3.0-generate", []string{"predict"}, false},
		{"gemini-2.5-flash-tts", chat, false},
		// Experimental snapshot without a clean version — drop.
		{"gemini-exp-1206", chat, false},
		// Non-Gemini chat model the endpoint may list — keep.
		{"learnlm-2.0-flash", chat, true},
	}
	for _, tc := range cases {
		if got := keepGeminiModel(tc.id, tc.methods); got != tc.keep {
			t.Errorf("keepGeminiModel(%q, %v) = %v, want %v", tc.id, tc.methods, got, tc.keep)
		}
	}
}

func TestKeepGeminiModelNoMethodsField(t *testing.T) {
	// If Google ever omits supportedGenerationMethods, we must not wipe the
	// whole list — only the id-based filters apply.
	if !keepGeminiModel("gemini-2.5-flash", nil) {
		t.Error("a current model with no methods field should still be kept")
	}
	if keepGeminiModel("text-embedding-004", nil) {
		t.Error("an embedding model should be dropped by id even without methods")
	}
	if keepGeminiModel("gemini-1.5-pro", nil) {
		t.Error("a deprecated generation should be dropped even without methods")
	}
}
