package security

import (
	"regexp"
)

// SecretScanner detects and redacts sensitive information.
type SecretScanner struct {
	patterns []*regexp.Regexp
}

func NewSecretScanner() *SecretScanner {
	return &SecretScanner{
		patterns: []*regexp.Regexp{
			// Example AWS key pattern
			regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`),
			// Example Generic Bearer token
			regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-\._~\+\/]+=*`),
		},
	}
}

// HasSecret returns true if the text contains a recognized secret.
func (s *SecretScanner) HasSecret(text string) bool {
	for _, p := range s.patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// Redact replaces all detected secrets with [REDACTED].
func (s *SecretScanner) Redact(text string) string {
	redacted := text
	for _, p := range s.patterns {
		redacted = p.ReplaceAllString(redacted, "[REDACTED]")
	}
	return redacted
}
