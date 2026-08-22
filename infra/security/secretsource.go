package security

// secretsource.go — resolving credentials from a password manager instead of
// a config file.
//
// Storing API keys in ~/.darkcode/config.json means they sit in plaintext,
// get copied into backups, and show up in a `cat`. A reference like
// "op://Private/OpenAI/credential" or "bw://openai-key" keeps the secret in
// the vault and fetches it at startup through the vendor's own CLI, so
// DarkCode never persists the value and the vault's unlock policy still
// applies.

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// secretTimeout bounds a vault lookup. A locked vault should fail fast rather
// than block startup on a CLI waiting for a prompt.
const secretTimeout = 15 * time.Second

// IsSecretRef reports whether a configured value is a vault reference rather
// than a literal secret.
func IsSecretRef(value string) bool {
	return strings.HasPrefix(value, "op://") || strings.HasPrefix(value, "bw://") ||
		strings.HasPrefix(value, "pass://")
}

// ResolveSecret turns a vault reference into its value. A value that is not a
// reference is returned unchanged, so callers can pass every credential
// through this without branching.
//
// Supported forms:
//
//	op://<vault>/<item>/<field>   1Password   (needs the `op` CLI, signed in)
//	bw://<item-id-or-name>        Bitwarden   (needs `bw`, BW_SESSION set)
//	pass://<path>                 pass(1)
func ResolveSecret(value string) (string, error) {
	switch {
	case strings.HasPrefix(value, "op://"):
		// `op read` takes the whole reference, including the scheme.
		return runSecretCLI("op", "read", "--no-newline", value)
	case strings.HasPrefix(value, "bw://"):
		return runSecretCLI("bw", "get", "password", strings.TrimPrefix(value, "bw://"))
	case strings.HasPrefix(value, "pass://"):
		return runSecretCLI("pass", "show", strings.TrimPrefix(value, "pass://"))
	default:
		return value, nil
	}
}

// runSecretCLI executes a password-manager CLI and returns its trimmed
// stdout. The secret never touches disk and is not logged; only the failure
// reason is surfaced.
func runSecretCLI(name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("%s CLI not found on PATH — install it or use a literal value", name)
	}
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(secretTimeout):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("%s timed out after %s — is the vault unlocked?", name, secretTimeout)
	}
	if err != nil {
		// Vendor CLIs put the useful diagnostic on stderr.
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("%s lookup failed: %s", name, msg)
	}
	secret := strings.TrimSpace(string(out))
	if secret == "" {
		return "", fmt.Errorf("%s returned an empty secret", name)
	}
	return secret, nil
}
