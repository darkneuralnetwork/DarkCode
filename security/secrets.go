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
			// AWS access key id.
			regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
			// Generic Bearer token in an Authorization header.
			regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-\._~\+\/]+=*`),
			// GitHub tokens (classic + fine-grained + OAuth/app/refresh).
			regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
			regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),
			// GitLab personal access token.
			regexp.MustCompile(`glpat-[A-Za-z0-9\-_]{20,}`),
			// Slack tokens.
			regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
			// Google API key.
			regexp.MustCompile(`AIza[0-9A-Za-z\-_]{35}`),
			// OpenAI / Anthropic style keys.
			regexp.MustCompile(`sk-[A-Za-z0-9\-_]{20,}`),
			// Stripe live/test secret keys.
			regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`),
			// Twilio account SID + auth pairing.
			regexp.MustCompile(`SK[0-9a-fA-F]{32}`),
			// PEM private key blocks (RSA/EC/OpenSSH/PGP).
			regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY-----`),
			// JWTs (three base64url segments).
			regexp.MustCompile(`eyJ[A-Za-z0-9\-_]+\.eyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+`),
			// AWS secret access key assigned to an obvious key variable. The
			// 40-char base64 anchor keeps this specific enough to avoid the
			// false positives a bare "password = ..." pattern would produce.
			regexp.MustCompile(`(?i)aws_secret_access_key["'\s:=]+[A-Za-z0-9/\+]{40}`),
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
