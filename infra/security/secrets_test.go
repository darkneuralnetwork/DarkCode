package security

import (
	"strings"
	"testing"
)

func TestSecretScannerHasSecret(t *testing.T) {
	s := NewSecretScanner()
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"aws access key", "key=AKIAIOSFODNN7EXAMPLE rest", true},
		{"bearer token", "Authorization: Bearer abc123.DEF-456_tok==", true},
		{"github classic pat", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789", true},
		{"github fine-grained", "github_pat_11ABCDEFG0aBcDeFgHiJkL_mNoPqRsTuVwXyZ", true},
		{"gitlab pat", "glpat-abcdefghij1234567890", true},
		{"slack token", "xoxb-123456789012-abcdefabcdef", true},
		{"google api key", "AIzaSyA1234567890abcdefghijklmnopqrstuvw", true},
		{"openai key", "sk-abcdefghijklmnopqrstuvwxyz1234", true},
		{"stripe live key", "sk_live_abcdefghijklmnop1234", true},
		{"pem private key", "-----BEGIN RSA PRIVATE KEY-----", true},
		{"jwt", "eyJhbGciOi.eyJzdWIiOiIx.SflKxwRJSMeKKF2QT4", true},
		{"benign prose", "the quick brown fox jumps over the lazy dog", false},
		{"lowercase akia not a full key", "akia is a river", false},
		{"benign sk word", "let me ask about your task list", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.HasSecret(tc.text); got != tc.want {
				t.Errorf("HasSecret(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestSecretScannerRedact(t *testing.T) {
	s := NewSecretScanner()
	in := "aws=AKIAIOSFODNN7EXAMPLE and auth Bearer sk-live-abc123=="
	out := s.Redact(in)
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("Redact left the AWS key in place: %q", out)
	}
	if strings.Contains(out, "sk-live-abc123") {
		t.Errorf("Redact left the bearer token in place: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("Redact did not insert the [REDACTED] marker: %q", out)
	}
	// Redacting clean text must be a no-op.
	clean := "nothing to see here"
	if got := s.Redact(clean); got != clean {
		t.Errorf("Redact(clean) = %q, want unchanged", got)
	}
}
